"""Rev360 reconciliation workbench core (SPEC 3 T3): legacy CSV/XML ingest,
Rev360-view simulator, defect-class rules engine, case ticketing, ETL."""

from __future__ import annotations

import csv
import hashlib
import io
import os
import sqlite3
import time
import xml.etree.ElementTree as ET
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

DATA_DIR = Path(os.environ.get("DATA_DIR", "/tmp/meridian-rev360"))
DB_PATH = DATA_DIR / "rev360.db"

TAX_TYPES = ("CIT", "PIT", "VAT", "WHT", "EDT", "CGT")

# ---------------------------------------------------------------- taxonomy
DEFECT_CLASSES: dict[str, dict[str, str]] = {
    "wrong_assessment": {
        "label": "Wrong assessment",
        "description": "Assessed amount in the legacy system differs from the "
                       "amount recorded in the Rev360 (NRS-side) view for the "
                       "same taxpayer/period/tax-type.",
    },
    "blocked_tcc": {
        "label": "Blocked TCC",
        "description": "Tax Clearance Certificate application blocked in "
                       "Rev360 although the legacy ledger shows no outstanding "
                       "liability for the taxpayer.",
    },
    "unrecognised_remittance": {
        "label": "Unrecognised remittance",
        "description": "A payment/remittance present in the legacy ledger has "
                       "no corresponding credit in the Rev360 view.",
    },
    "duplicate_payment": {
        "label": "Duplicate payment",
        "description": "The same payment reference is posted more than once "
                       "(double credit risk / reconciliation break).",
    },
    "tin_mismatch": {
        "label": "TIN mismatch",
        "description": "Taxpayer identifier differs between legacy and "
                       "Rev360 views (formatting, transposition or wrong TIN).",
    },
}


def severity_for(defect_class: str, detail: dict) -> str:
    """Severity rubric per defect class."""
    if defect_class == "wrong_assessment":
        legacy = abs(int(detail.get("legacy_amount_kobo", 0)))
        rev = abs(int(detail.get("rev360_amount_kobo", 0)))
        base = max(legacy, rev, 1)
        ratio = abs(legacy - rev) / base
        if ratio > 0.50:
            return "critical"
        if ratio > 0.10:
            return "high"
        return "medium"
    if defect_class == "blocked_tcc":
        return "high"
    if defect_class == "unrecognised_remittance":
        amount = abs(int(detail.get("amount_kobo", 0)))
        return "critical" if amount >= 10_000_000_00 else "high"  # >= N10m
    if defect_class == "duplicate_payment":
        return "medium"
    if defect_class == "tin_mismatch":
        return "low"
    return "medium"


# ---------------------------------------------------------------- ingest
@dataclass
class LegacyRecord:
    record_id: str
    tin: str
    taxpayer_name: str
    tax_type: str
    period: str           # YYYY-MM
    amount_kobo: int      # assessment amount
    payment_ref: str = ""
    payment_kobo: int = 0
    assessment_ref: str = ""
    tcc_ref: str = ""
    record_date: str = ""

    def key(self) -> str:
        return f"{normalise_tin(self.tin)}|{self.tax_type}|{self.period}"


def normalise_tin(tin: str) -> str:
    return "".join(c for c in (tin or "") if c.isdigit())


def parse_csv_records(text: str) -> list[LegacyRecord]:
    reader = csv.DictReader(io.StringIO(text))
    required = {"record_id", "tin", "taxpayer_name", "tax_type", "period",
                "amount_kobo"}
    missing = required - set(reader.fieldnames or [])
    if missing:
        raise ValueError(f"CSV missing columns: {sorted(missing)}")
    out = []
    for i, row in enumerate(reader, start=2):
        try:
            out.append(LegacyRecord(
                record_id=row["record_id"].strip(),
                tin=row["tin"].strip(),
                taxpayer_name=row["taxpayer_name"].strip(),
                tax_type=row["tax_type"].strip().upper(),
                period=row["period"].strip(),
                amount_kobo=int(row["amount_kobo"]),
                payment_ref=(row.get("payment_ref") or "").strip(),
                payment_kobo=int(row.get("payment_kobo") or 0),
                assessment_ref=(row.get("assessment_ref") or "").strip(),
                tcc_ref=(row.get("tcc_ref") or "").strip(),
                record_date=(row.get("record_date") or "").strip(),
            ))
        except (KeyError, ValueError) as exc:
            raise ValueError(f"CSV row {i}: {exc}") from exc
    return out


def parse_xml_records(text: str) -> list[LegacyRecord]:
    """Legacy XML shape: <ledger><record ...><field>value</field>...</record></ledger>"""
    try:
        root = ET.fromstring(text)
    except ET.ParseError as exc:
        raise ValueError(f"invalid XML: {exc}") from exc
    out = []
    for rec in root.iter("record"):
        fields = {child.tag: (child.text or "").strip() for child in rec}
        fields.update({k: v for k, v in rec.attrib.items()})
        try:
            out.append(LegacyRecord(
                record_id=fields["record_id"], tin=fields.get("tin", ""),
                taxpayer_name=fields.get("taxpayer_name", ""),
                tax_type=fields.get("tax_type", "").upper(),
                period=fields.get("period", ""),
                amount_kobo=int(fields.get("amount_kobo") or 0),
                payment_ref=fields.get("payment_ref", ""),
                payment_kobo=int(fields.get("payment_kobo") or 0),
                assessment_ref=fields.get("assessment_ref", ""),
                tcc_ref=fields.get("tcc_ref", ""),
                record_date=fields.get("record_date", ""),
            ))
        except (KeyError, ValueError) as exc:
            raise ValueError(f"XML record: {exc}") from exc
    if not out:
        raise ValueError("no <record> elements found")
    return out


# ---------------------------------------------------------------- store
SCHEMA = """
CREATE TABLE IF NOT EXISTS legacy_records (
  record_id TEXT PRIMARY KEY, tin TEXT, taxpayer_name TEXT, tax_type TEXT,
  period TEXT, amount_kobo INTEGER, payment_ref TEXT, payment_kobo INTEGER,
  assessment_ref TEXT, tcc_ref TEXT, record_date TEXT, batch_id TEXT);
CREATE TABLE IF NOT EXISTS defects (
  id TEXT PRIMARY KEY, defect_class TEXT, severity TEXT, record_key TEXT,
  detail TEXT, status TEXT, detected_at TEXT, run_id TEXT);
CREATE TABLE IF NOT EXISTS cases (
  id TEXT PRIMARY KEY, title TEXT, defect_id TEXT, assignee TEXT,
  status TEXT, priority TEXT, notes TEXT, created_at TEXT, updated_at TEXT,
  worm_evidence_id TEXT);
CREATE TABLE IF NOT EXISTS etl_staging (record_id TEXT PRIMARY KEY, payload TEXT, batch_id TEXT);
CREATE TABLE IF NOT EXISTS etl_clean (record_id TEXT PRIMARY KEY, payload TEXT, batch_id TEXT);
CREATE TABLE IF NOT EXISTS runs (run_id TEXT PRIMARY KEY, kind TEXT, summary TEXT, created_at TEXT);
"""


def db() -> sqlite3.Connection:
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(DB_PATH)
    conn.row_factory = sqlite3.Row
    conn.executescript(SCHEMA)
    return conn


def now() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


# ------------------------------------------------------- Rev360 simulator
def _bucket(*parts: str) -> int:
    h = hashlib.sha256("|".join(parts).encode()).hexdigest()
    return int(h[:8], 16) % 100


def rev360_view(records: list[LegacyRecord]) -> list[dict]:
    """Deterministic NRS-side Rev360 view of the legacy ledger.

    The simulator models what the NRS Rev360 system shows for the same
    taxpayers, introducing realistic discrepancies (this is the system under
    reconciliation — SIMULATED, tagged in README).
    """
    view = []
    for r in records:
        entry: dict[str, Any] = {
            "tin": r.tin, "taxpayer_name": r.taxpayer_name,
            "tax_type": r.tax_type, "period": r.period,
            "assessed_amount_kobo": r.amount_kobo,
            "payment_ref": r.payment_ref,
            "credited_kobo": r.payment_kobo,
            "tcc_status": "clear",
            "record_key": r.key(),
        }
        b = _bucket(r.record_id, r.tin)
        if r.amount_kobo and b < 18:
            # wrong assessment: NRS side shows a different figure
            entry["assessed_amount_kobo"] = int(r.amount_kobo * (1 + ((b % 5) + 1) / 10))
        if b < 12:
            entry["tcc_status"] = "blocked"
        if r.payment_ref and b < 20:
            entry["payment_ref"] = ""      # remittance unrecognised NRS-side
            entry["credited_kobo"] = 0
        if 20 <= b < 28 and normalise_tin(r.tin):
            tin = normalise_tin(r.tin)
            entry["tin"] = "0" + tin       # formatting mismatch
        view.append(entry)
    return view


# ------------------------------------------------------- defect detection
@dataclass
class Defect:
    id: str
    defect_class: str
    severity: str
    record_key: str
    detail: dict
    status: str = "open"
    detected_at: str = field(default_factory=now)
    run_id: str = ""


def detect_defects(records: list[LegacyRecord], run_id: str) -> list[Defect]:
    """Defect-class rules engine: compare legacy vs Rev360 view."""
    import json as _json  # local to keep detail serialisable

    view = {e["record_key"]: e for e in rev360_view(records)}
    defects: list[Defect] = []
    seen_payment_refs: dict[str, str] = {}

    def mk(cls: str, key: str, detail: dict) -> Defect:
        did = hashlib.sha256(_json.dumps(
            {"c": cls, "k": key, "d": detail}, sort_keys=True).encode()).hexdigest()[:16]
        return Defect(id=f"def-{did}", defect_class=cls,
                      severity=severity_for(cls, detail), record_key=key,
                      detail=detail, run_id=run_id)

    for r in records:
        e = view.get(r.key())
        if e is None:
            continue
        # tin_mismatch
        if normalise_tin(e["tin"]) != normalise_tin(r.tin):
            defects.append(mk("tin_mismatch", r.key(), {
                "legacy_tin": r.tin, "rev360_tin": e["tin"],
                "taxpayer_name": r.taxpayer_name}))
        # wrong_assessment
        if e["assessed_amount_kobo"] != r.amount_kobo:
            defects.append(mk("wrong_assessment", r.key(), {
                "legacy_amount_kobo": r.amount_kobo,
                "rev360_amount_kobo": e["assessed_amount_kobo"],
                "assessment_ref": r.assessment_ref}))
        # unrecognised_remittance
        if r.payment_ref and not e["payment_ref"]:
            defects.append(mk("unrecognised_remittance", r.key(), {
                "payment_ref": r.payment_ref, "amount_kobo": r.payment_kobo}))
        # blocked_tcc: blocked in Rev360 while legacy shows settled liability
        if e["tcc_status"] == "blocked" and r.tcc_ref:
            defects.append(mk("blocked_tcc", r.key(), {
                "tcc_ref": r.tcc_ref, "rev360_status": "blocked",
                "outstanding_kobo": 0}))
        # duplicate_payment (same payment ref twice in legacy)
        if r.payment_ref:
            if r.payment_ref in seen_payment_refs:
                defects.append(mk("duplicate_payment", r.key(), {
                    "payment_ref": r.payment_ref,
                    "first_record": seen_payment_refs[r.payment_ref],
                    "duplicate_record": r.record_id,
                    "amount_kobo": r.payment_kobo}))
            else:
                seen_payment_refs[r.payment_ref] = r.record_id
    return defects
