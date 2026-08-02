"""Durable store (SPEC §1.3).

Selection (HARDENING.md H1): DATABASE_URL set -> Postgres via psycopg[binary]
(profile=prod); unset -> SQLite file at DATA_DIR/etr.db (profile=dev). Table
schemas are identical across both backends; DDL is idempotent
(CREATE TABLE IF NOT EXISTS). Startup never fails because a prod var is
missing — if Postgres is unreachable the store falls back to SQLite.
"""
from __future__ import annotations

import json
import logging
import os
import sqlite3
import threading
from pathlib import Path
from typing import Any

from .models import Computation, ConstituentEntity, Group

log = logging.getLogger("etr.store")

_SQLITE_DDL = """
CREATE TABLE IF NOT EXISTS groups(
    id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS entities(
    id TEXT PRIMARY KEY, group_id TEXT NOT NULL, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS computations(
    id TEXT PRIMARY KEY, group_id TEXT NOT NULL, fiscal_year INT, data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS ix_etr_entities_group ON entities(group_id);
CREATE INDEX IF NOT EXISTS ix_etr_comp_group ON computations(group_id);
CREATE UNIQUE INDEX IF NOT EXISTS ux_etr_comp_group_year ON computations(group_id, fiscal_year);
"""

# Same logical schema; `seq` gives the computations ordering that SQLite
# provides via rowid.
_PG_DDL = """
CREATE TABLE IF NOT EXISTS groups(
    id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS entities(
    id TEXT PRIMARY KEY, group_id TEXT NOT NULL, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS computations(
    id TEXT PRIMARY KEY, group_id TEXT NOT NULL, fiscal_year INT, data TEXT NOT NULL,
    seq BIGSERIAL);
CREATE INDEX IF NOT EXISTS ix_etr_entities_group ON entities(group_id);
CREATE INDEX IF NOT EXISTS ix_etr_comp_group ON computations(group_id);
CREATE UNIQUE INDEX IF NOT EXISTS ux_etr_comp_group_year ON computations(group_id, fiscal_year);
"""


class _SQLiteBackend:
    kind = "sqlite"

    def __init__(self, path: str | None = None) -> None:
        db = path or os.environ.get(
            "ETR_DB", os.path.join(os.environ.get("DATA_DIR", "./data"), "etr.db"))
        Path(db).parent.mkdir(parents=True, exist_ok=True)
        self.conn = sqlite3.connect(db, check_same_thread=False)
        self.conn.row_factory = sqlite3.Row
        with self.conn:
            self.conn.executescript(_SQLITE_DDL)
        log.info("profile=dev component=store (sqlite %s)", db)

    @staticmethod
    def _sql(q: str) -> str:
        return q.replace("%s", "?").replace(
            "ON CONFLICT (id) DO UPDATE SET data=EXCLUDED.data",
            "ON CONFLICT(id) DO UPDATE SET data=excluded.data").replace(
            "ON CONFLICT (id) DO UPDATE SET group_id=EXCLUDED.group_id, fiscal_year=EXCLUDED.fiscal_year, data=EXCLUDED.data",
            "ON CONFLICT(id) DO UPDATE SET group_id=excluded.group_id, fiscal_year=excluded.fiscal_year, data=excluded.data")

    def execute(self, q: str, params: tuple = ()) -> Any:
        with self.conn:
            cur = self.conn.execute(self._sql(q), params)
            return cur.fetchall()

    def execute_one(self, q: str, params: tuple = ()) -> Any:
        rows = self.execute(q, params)
        return rows[0] if rows else None

    @staticmethod
    def _col(row: Any, name: str, idx: int) -> Any:
        return row[name]


class _PostgresBackend:
    kind = "postgres"

    def __init__(self, dsn: str) -> None:
        import psycopg  # psycopg[binary], imported lazily so dev needs nothing

        from psycopg.rows import dict_row
        self.conn = psycopg.connect(dsn, autocommit=True, row_factory=dict_row)
        with self.conn.cursor() as cur:
            for stmt in _PG_DDL.split(";"):
                if stmt.strip():
                    cur.execute(stmt)
        log.info("profile=prod component=store (postgres)")

    def execute(self, q: str, params: tuple = ()) -> Any:
        with self.conn.cursor() as cur:
            cur.execute(q, params)
            return cur.fetchall() if cur.description else []

    def execute_one(self, q: str, params: tuple = ()) -> Any:
        rows = self.execute(q, params)
        return rows[0] if rows else None

    @staticmethod
    def _col(row: Any, name: str, idx: int) -> Any:
        return row[name]


_UPSERT_PLAIN = ("INSERT INTO {table}(id, data) VALUES(%s,%s) "
                 "ON CONFLICT (id) DO UPDATE SET data=EXCLUDED.data")
_UPSERT_ENTITY = ("INSERT INTO entities(id, group_id, data) VALUES(%s,%s,%s) "
                  "ON CONFLICT (id) DO UPDATE SET data=EXCLUDED.data")
_UPSERT_COMP = ("INSERT INTO computations(id, group_id, fiscal_year, data) VALUES(%s,%s,%s,%s) "
                "ON CONFLICT (id) DO UPDATE SET group_id=EXCLUDED.group_id, "
                "fiscal_year=EXCLUDED.fiscal_year, data=EXCLUDED.data")


class Store:
    """Document store with the same interface on SQLite (dev) and Postgres
    (prod, DATABASE_URL)."""

    def __init__(self, path: str | None = None) -> None:
        self._lock = threading.Lock()
        dsn = os.environ.get("DATABASE_URL", "")
        if dsn and path is None:
            try:
                self._b: Any = _PostgresBackend(dsn)
                return
            except Exception as e:  # pragma: no cover - needs real pg
                log.warning("postgres unavailable (%s); falling back to sqlite", e)
        self._b = _SQLiteBackend(path)

    @property
    def backend(self) -> str:
        return self._b.kind

    # ---- groups ----
    def put_group(self, g: Group) -> None:
        with self._lock:
            self._b.execute(_UPSERT_PLAIN.format(table="groups"), (g.id, g.model_dump_json()))

    def get_group(self, gid: str) -> Group | None:
        row = self._b.execute_one("SELECT data FROM groups WHERE id=%s", (gid,))
        return Group.model_validate_json(row["data"]) if row else None

    # ---- entities ----
    def put_entities(self, ents: list[ConstituentEntity]) -> int:
        with self._lock:
            for e in ents:
                self._b.execute(_UPSERT_ENTITY, (e.id, e.group_id, e.model_dump_json()))
        return len(ents)

    def entities(self, group_id: str) -> list[ConstituentEntity]:
        rows = self._b.execute("SELECT data FROM entities WHERE group_id=%s", (group_id,))
        return [ConstituentEntity.model_validate_json(r["data"]) for r in rows]

    def all_entities(self) -> list[ConstituentEntity]:
        rows = self._b.execute("SELECT data FROM entities")
        return [ConstituentEntity.model_validate_json(r["data"]) for r in rows]

    # ---- computations ----
    def put_computation(self, c: Computation) -> None:
        with self._lock:
            # one live computation per (group_id, fiscal_year) — enforced by
            # ux_etr_comp_group_year; a recomputation supersedes the prior
            # record (last write wins, same as the doc-store convention)
            self._b.execute(
                "DELETE FROM computations WHERE group_id=%s AND fiscal_year=%s AND id<>%s",
                (c.group_id, c.fiscal_year, c.id))
            self._b.execute(_UPSERT_COMP, (c.id, c.group_id, c.fiscal_year, c.model_dump_json()))

    def get_computation(self, cid: str) -> Computation | None:
        row = self._b.execute_one("SELECT data FROM computations WHERE id=%s", (cid,))
        return Computation.model_validate_json(row["data"]) if row else None

    def list_computations(self, group_id: str = "") -> list[dict[str, Any]]:
        if self._b.kind == "postgres":
            order = "seq"
        else:
            order = "rowid"
        if group_id:
            rows = self._b.execute(
                f"SELECT data FROM computations WHERE group_id=%s ORDER BY {order} DESC", (group_id,))
        else:
            rows = self._b.execute(f"SELECT data FROM computations ORDER BY {order} DESC")
        out = []
        for r in rows:
            d = json.loads(r["data"])
            out.append({"id": d["id"], "group_id": d["group_id"], "fiscal_year": d["fiscal_year"],
                        "basis": d["basis"], "created_at": d["created_at"],
                        "total_topup_kobo": d["total_topup_kobo"], "in_scope": d["in_scope"],
                        "digest": d.get("digest", "")})
        return out
