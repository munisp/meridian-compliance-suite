"""SQLite-backed store (SPEC §1.3: SQLite fallback when DATABASE_URL unset)."""
from __future__ import annotations

import json
import os
import sqlite3
import threading
from pathlib import Path
from typing import Any

from .models import Computation, ConstituentEntity, Group


class Store:
    def __init__(self, path: str | None = None) -> None:
        db = path or os.environ.get("ETR_DB", os.path.join(os.environ.get("DATA_DIR", "./data"), "etr.db"))
        Path(db).parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()
        self.conn = sqlite3.connect(db, check_same_thread=False)
        self.conn.row_factory = sqlite3.Row
        with self._lock, self.conn:
            self.conn.executescript("""
            CREATE TABLE IF NOT EXISTS groups(
                id TEXT PRIMARY KEY, data TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS entities(
                id TEXT PRIMARY KEY, group_id TEXT NOT NULL, data TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS computations(
                id TEXT PRIMARY KEY, group_id TEXT NOT NULL, fiscal_year INT, data TEXT NOT NULL);
            """)

    # ---- groups ----
    def put_group(self, g: Group) -> None:
        with self._lock, self.conn:
            self.conn.execute("INSERT OR REPLACE INTO groups(id, data) VALUES(?,?)",
                              (g.id, g.model_dump_json()))

    def get_group(self, gid: str) -> Group | None:
        row = self.conn.execute("SELECT data FROM groups WHERE id=?", (gid,)).fetchone()
        return Group.model_validate_json(row["data"]) if row else None

    # ---- entities ----
    def put_entities(self, ents: list[ConstituentEntity]) -> int:
        with self._lock, self.conn:
            for e in ents:
                self.conn.execute(
                    "INSERT OR REPLACE INTO entities(id, group_id, data) VALUES(?,?,?)",
                    (e.id, e.group_id, e.model_dump_json()))
        return len(ents)

    def entities(self, group_id: str) -> list[ConstituentEntity]:
        rows = self.conn.execute("SELECT data FROM entities WHERE group_id=?", (group_id,)).fetchall()
        return [ConstituentEntity.model_validate_json(r["data"]) for r in rows]

    def all_entities(self) -> list[ConstituentEntity]:
        rows = self.conn.execute("SELECT data FROM entities").fetchall()
        return [ConstituentEntity.model_validate_json(r["data"]) for r in rows]

    # ---- computations ----
    def put_computation(self, c: Computation) -> None:
        with self._lock, self.conn:
            self.conn.execute(
                "INSERT OR REPLACE INTO computations(id, group_id, fiscal_year, data) VALUES(?,?,?,?)",
                (c.id, c.group_id, c.fiscal_year, c.model_dump_json()))

    def get_computation(self, cid: str) -> Computation | None:
        row = self.conn.execute("SELECT data FROM computations WHERE id=?", (cid,)).fetchone()
        return Computation.model_validate_json(row["data"]) if row else None

    def list_computations(self, group_id: str = "") -> list[dict[str, Any]]:
        if group_id:
            rows = self.conn.execute(
                "SELECT data FROM computations WHERE group_id=? ORDER BY rowid DESC", (group_id,)).fetchall()
        else:
            rows = self.conn.execute("SELECT data FROM computations ORDER BY rowid DESC").fetchall()
        out = []
        for r in rows:
            d = json.loads(r["data"])
            out.append({"id": d["id"], "group_id": d["group_id"], "fiscal_year": d["fiscal_year"],
                        "basis": d["basis"], "created_at": d["created_at"],
                        "total_topup_kobo": d["total_topup_kobo"], "in_scope": d["in_scope"],
                        "digest": d.get("digest", "")})
        return out
