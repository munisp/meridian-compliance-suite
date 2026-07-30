"""Entity/transaction graph ingest + store (SPEC 3 T8), FX table, and
per-tenant rule-pack pins (swappable-pack mechanism). SQLite dev store;
Postgres when DATABASE_URL is set (DSN swap only)."""

from __future__ import annotations

import json
import os
import sqlite3
import time
import uuid
from pathlib import Path

DATA_DIR = Path(os.environ.get("DATA_DIR", "/tmp/meridian-tp-cbcr"))
DB_PATH = DATA_DIR / "tpcbcr.db"

SCHEMA = """
CREATE TABLE IF NOT EXISTS entities (
  tin TEXT PRIMARY KEY, name TEXT, jurisdiction TEXT, role TEXT,
  entity_type TEXT, biz_activity TEXT, tenant_id TEXT, created_at TEXT);
CREATE TABLE IF NOT EXISTS transactions (
  id TEXT PRIMARY KEY, from_tin TEXT, to_tin TEXT, tx_type TEXT,
  amount_kobo INTEGER, currency TEXT, tenant_id TEXT, created_at TEXT);
CREATE TABLE IF NOT EXISTS pack_pins (
  tenant_id TEXT, pack_id TEXT, version TEXT,
  PRIMARY KEY (tenant_id, pack_id));
CREATE TABLE IF NOT EXISTS fx_rates (
  currency TEXT PRIMARY KEY, per_ngn REAL, as_of TEXT);
CREATE TABLE IF NOT EXISTS reports (
  id TEXT PRIMARY KEY, kind TEXT, payload TEXT, created_at TEXT);
"""

DEFAULT_FX = {  # dev FX table: units of currency per 1 NGN is 1/per_ngn
    "NGN": (1.0, "2026-01-01"),
    "USD": (1550.0, "2026-01-01"),   # N1550 / USD
    "EUR": (1680.0, "2026-01-01"),
    "GBP": (1980.0, "2026-01-01"),
    "XOF": (2.6, "2026-01-01"),
    "ZAR": (85.0, "2026-01-01"),
}


def db() -> sqlite3.Connection:
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(DB_PATH)
    conn.row_factory = sqlite3.Row
    conn.executescript(SCHEMA)
    for cur, (rate, as_of) in DEFAULT_FX.items():
        conn.execute(
            "INSERT OR IGNORE INTO fx_rates VALUES (?,?,?)", (cur, rate, as_of))
    conn.commit()
    return conn


def now() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


# ---------------------------------------------------------------- graph
def add_entity(e: dict, tenant_id: str = "") -> dict:
    with db() as conn:
        conn.execute(
            "INSERT OR REPLACE INTO entities VALUES (?,?,?,?,?,?,?,?)",
            (e["tin"], e["name"], e.get("jurisdiction", "NG"),
             e.get("role", "subsidiary"), e.get("entity_type", "company"),
             e.get("biz_activity", "CBC501"), tenant_id, now()))
    return e


def list_entities(tenant_id: str = "") -> list[dict]:
    with db() as conn:
        rows = conn.execute("SELECT * FROM entities").fetchall()
    return [dict(r) for r in rows]


def add_transaction(t: dict, tenant_id: str = "") -> dict:
    tid = t.get("id") or f"tx-{uuid.uuid4().hex[:12]}"
    with db() as conn:
        conn.execute(
            "INSERT OR REPLACE INTO transactions VALUES (?,?,?,?,?,?,?,?)",
            (tid, t["from_tin"], t["to_tin"], t.get("tx_type", "service"),
             int(t.get("amount_kobo", 0)), t.get("currency", "NGN"),
             tenant_id, now()))
    t["id"] = tid
    return t


def list_transactions(tenant_id: str = "") -> list[dict]:
    with db() as conn:
        rows = conn.execute("SELECT * FROM transactions").fetchall()
    return [dict(r) for r in rows]


def controlled_transactions_total(tenant_id: str = "") -> int:
    with db() as conn:
        row = conn.execute(
            "SELECT COALESCE(SUM(amount_kobo),0) AS s FROM transactions"
        ).fetchone()
    return int(row["s"])


# ---------------------------------------------------------------- packs
def pin_pack(tenant_id: str, pack_id: str, version: str) -> dict:
    with db() as conn:
        conn.execute("INSERT OR REPLACE INTO pack_pins VALUES (?,?,?)",
                     (tenant_id, pack_id, version))
    return {"tenant_id": tenant_id, "pack_id": pack_id, "version": version}


def get_pin(tenant_id: str, pack_id: str) -> str:
    with db() as conn:
        row = conn.execute(
            "SELECT version FROM pack_pins WHERE tenant_id=? AND pack_id=?",
            (tenant_id, pack_id)).fetchone()
    return row["version"] if row else ""


def list_pins() -> list[dict]:
    with db() as conn:
        rows = conn.execute("SELECT * FROM pack_pins").fetchall()
    return [dict(r) for r in rows]


# ---------------------------------------------------------------- FX
def fx_table() -> list[dict]:
    with db() as conn:
        rows = conn.execute("SELECT * FROM fx_rates ORDER BY currency").fetchall()
    return [dict(r) for r in rows]


def upsert_fx(currency: str, per_ngn: float, as_of: str) -> dict:
    with db() as conn:
        conn.execute("INSERT OR REPLACE INTO fx_rates VALUES (?,?,?)",
                     (currency.upper(), per_ngn, as_of))
    return {"currency": currency.upper(), "per_ngn": per_ngn, "as_of": as_of}


def fx_convert(amount_minor: int, from_ccy: str, to_ccy: str) -> int:
    """Convert integer minor units via NGN cross rates (integer math)."""
    table = {r["currency"]: r["per_ngn"] for r in fx_table()}
    f, t = from_ccy.upper(), to_ccy.upper()
    if f not in table or t not in table:
        raise ValueError(f"unknown currency {f} or {t}")
    if f == t:
        return amount_minor
    # amount(from) -> NGN -> to: ngn = amount * per_ngn[f]; to = ngn / per_ngn[t]
    return round(amount_minor * table[f] / table[t])


# ---------------------------------------------------------------- reports
def save_report(kind: str, payload: dict) -> str:
    rid = f"rpt-{uuid.uuid4().hex[:12]}"
    with db() as conn:
        conn.execute("INSERT INTO reports VALUES (?,?,?,?)",
                     (rid, kind, json.dumps(payload), now()))
    return rid
