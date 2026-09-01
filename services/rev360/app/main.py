"""Rev360 reconciliation workbench API (SPEC 3 T3).

Endpoints:
  GET  /healthz, /readyz
  GET  /v1/sso/login                     consultant OIDC dev SSO (issues JWT)
  POST /v1/ingest                        legacy CSV/XML ingest
  GET  /v1/records                       ingested legacy records
  GET  /v1/rev360/view                   Rev360-view simulator output
  POST /v1/reconcile/run                 run defect-class rules engine
  GET  /v1/defects                       list defects (?class=&severity=&status=)
  POST /v1/cases                         create case ticket
  GET  /v1/cases, /v1/cases/{id}         case read
  PATCH /v1/cases/{id}                   update case
  POST /v1/cases/{id}/resolve            resolve + WORM evidence per corrected record
  POST /v1/etl/extract, /v1/etl/load     controlled ETL
  GET  /v1/evidence/{id}/verify          verify local WORM object
"""

from __future__ import annotations

import json
import sqlite3
import uuid
from typing import Optional

from fastapi import Depends, FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse
from pydantic import BaseModel

from meridian_py.dev_jwt import (AuthDep, issue_token, problem,
                                 validate_auth_config)

from . import core, worm

# Fail closed at startup when AUTH_MODE=keycloak is missing OIDC config.
validate_auth_config()

SERVICE = "rev360"
VERSION = "1.0.0"

app = FastAPI(title="Meridian Rev360 Workbench", version=VERSION)


# OTel bootstrap (DESIGN-CONTRACT.md): fail-soft, never breaks startup or
# money paths. Instruments FastAPI + outbound httpx/requests; tenant.id is
# stamped on the active span + baggage. Authz/tenant guards untouched.
from meridian_py.otel import TenantBaggageMiddleware, init_otel

init_otel(app)
app.add_middleware(TenantBaggageMiddleware)


@app.exception_handler(HTTPException)
async def http_exc_handler(_: Request, exc: HTTPException):
    return problem(exc.status_code, str(exc.detail), "")


@app.get("/healthz")
def healthz():
    return {"status": "ok", "service": SERVICE, "version": VERSION}


@app.get("/readyz")
def readyz():
    with core.db() as conn:
        conn.execute("SELECT 1")
    return {"status": "ready"}


# ------------------------------------------------------------- SSO (dev)
@app.get("/v1/sso/login")
def sso_login(consultant: str = "consultant@meridian.local", role: str = "operator",
              tenant_id: str = ""):
    """Consultant OIDC dev SSO: simulates the OIDC authorization-code
    callback and issues a session JWT.

    WARNING: DEV ONLY. This endpoint mints tokens with caller-chosen role and
    tenant with no authentication; it exists solely for local development and
    is disabled (404) unless AUTH_MODE=dev. In production, consultants sign
    in via Keycloak OIDC and receive RS256 tokens from the realm."""
    import os
    if os.environ.get("AUTH_MODE", "dev") != "dev":
        return problem(404, "not found",
                       "dev SSO simulator is disabled unless AUTH_MODE=dev")
    if role not in ("admin", "operator", "auditor"):
        return problem(400, "bad role", "admin|operator|auditor")
    token = issue_token(sub=consultant, roles=[role], tenant_id=tenant_id)
    return {"token": token, "token_type": "Bearer", "via": "dev-oidc-simulator"}


# ------------------------------------------------------------- ingest
@app.post("/v1/ingest", status_code=201)
async def ingest(request: Request, principal=AuthDep):
    body = (await request.body()).decode()
    ctype = request.headers.get("content-type", "")
    try:
        if "xml" in ctype or body.lstrip().startswith("<"):
            records = core.parse_xml_records(body)
            fmt = "xml"
        else:
            records = core.parse_csv_records(body)
            fmt = "csv"
    except ValueError as exc:
        return problem(422, "ingest failed", str(exc))
    batch_id = f"batch-{uuid.uuid4().hex[:12]}"
    with core.db() as conn:
        for r in records:
            conn.execute(
                "INSERT OR REPLACE INTO legacy_records VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
                (r.record_id, r.tin, r.taxpayer_name, r.tax_type, r.period,
                 r.amount_kobo, r.payment_ref, r.payment_kobo, r.assessment_ref,
                 r.tcc_ref, r.record_date, batch_id))
    return {"batch_id": batch_id, "format": fmt, "ingested": len(records)}


def _all_records() -> list[core.LegacyRecord]:
    with core.db() as conn:
        rows = conn.execute("SELECT * FROM legacy_records").fetchall()
    return [core.LegacyRecord(**{k: row[k] for k in (
        "record_id", "tin", "taxpayer_name", "tax_type", "period",
        "amount_kobo", "payment_ref", "payment_kobo", "assessment_ref",
        "tcc_ref", "record_date")}) for row in rows]


@app.get("/v1/records")
def records(principal=AuthDep):
    recs = _all_records()
    return {"count": len(recs), "records": [r.__dict__ for r in recs]}


@app.get("/v1/rev360/view")
def rev360_view(principal=AuthDep):
    """SIMULATED NRS-side Rev360 dataset (adapter behind interface)."""
    view = core.rev360_view(_all_records())
    return {"simulated": True, "count": len(view), "entries": view}


# ------------------------------------------------------------- reconcile
@app.post("/v1/reconcile/run", status_code=201)
def reconcile_run(principal=AuthDep):
    run_id = f"run-{uuid.uuid4().hex[:12]}"
    recs = _all_records()
    defects = core.detect_defects(recs, run_id)
    with core.db() as conn:
        conn.execute("DELETE FROM defects")  # latest run is authoritative
        for d in defects:
            conn.execute(
                "INSERT INTO defects VALUES (?,?,?,?,?,?,?,?)",
                (d.id, d.defect_class, d.severity, d.record_key,
                 json.dumps(d.detail), d.status, d.detected_at, d.run_id))
        summary = {"records": len(recs), "defects": len(defects),
                   "by_class": {}}
        for d in defects:
            summary["by_class"][d.defect_class] = \
                summary["by_class"].get(d.defect_class, 0) + 1
        conn.execute("INSERT INTO runs VALUES (?,?,?,?)",
                     (run_id, "reconcile", json.dumps(summary), core.now()))
    return {"run_id": run_id, **summary}


@app.get("/v1/defects")
def list_defects(defect_class: Optional[str] = None,
                 severity: Optional[str] = None,
                 status: Optional[str] = None,
                 principal=AuthDep):
    q = "SELECT * FROM defects WHERE 1=1"
    args: list = []
    if defect_class:
        q += " AND defect_class=?"; args.append(defect_class)
    if severity:
        q += " AND severity=?"; args.append(severity)
    if status:
        q += " AND status=?"; args.append(status)
    with core.db() as conn:
        rows = conn.execute(q, args).fetchall()
    out = [dict(row) | {"detail": json.loads(row["detail"])} for row in rows]
    return {"count": len(out), "defects": out,
            "taxonomy": core.DEFECT_CLASSES}


# ------------------------------------------------------------- cases
class CaseIn(BaseModel):
    title: str
    defect_id: str = ""
    assignee: str = ""
    priority: str = "medium"
    notes: str = ""


class CasePatch(BaseModel):
    title: Optional[str] = None
    assignee: Optional[str] = None
    status: Optional[str] = None
    priority: Optional[str] = None
    notes: Optional[str] = None


@app.post("/v1/cases", status_code=201)
def create_case(body: CaseIn, principal=AuthDep):
    cid = f"case-{uuid.uuid4().hex[:12]}"
    ts = core.now()
    with core.db() as conn:
        conn.execute("INSERT INTO cases VALUES (?,?,?,?,?,?,?,?,?,?)",
                     (cid, body.title, body.defect_id, body.assignee,
                      "open", body.priority, body.notes, ts, ts, ""))
    return _case(cid)


@app.get("/v1/cases")
def list_cases(status: Optional[str] = None, principal=AuthDep):
    q = "SELECT * FROM cases" + (" WHERE status=?" if status else "")
    with core.db() as conn:
        rows = conn.execute(q, (status,) if status else ()).fetchall()
    return {"count": len(rows), "cases": [dict(r) for r in rows]}


def _case(cid: str) -> dict:
    with core.db() as conn:
        row = conn.execute("SELECT * FROM cases WHERE id=?", (cid,)).fetchone()
    if row is None:
        raise HTTPException(404, f"case {cid} not found")
    return dict(row)


@app.get("/v1/cases/{cid}")
def get_case(cid: str, principal=AuthDep):
    return _case(cid)


@app.patch("/v1/cases/{cid}")
def patch_case(cid: str, body: CasePatch, principal=AuthDep):
    case = _case(cid)
    updates = {k: v for k, v in body.model_dump().items() if v is not None}
    if updates:
        sets = ", ".join(f"{k}=?" for k in updates)
        with core.db() as conn:
            conn.execute(f"UPDATE cases SET {sets}, updated_at=? WHERE id=?",
                         (*updates.values(), core.now(), cid))
    return _case(cid) if updates else case


class ResolveIn(BaseModel):
    correction: dict
    note: str = ""


@app.post("/v1/cases/{cid}/resolve")
def resolve_case(cid: str, body: ResolveIn, principal=AuthDep):
    """Resolve a case: apply the correction and write WORM evidence for the
    corrected record (audit-evidence API or local WORM fallback)."""
    case = _case(cid)
    if case["status"] == "resolved":
        return problem(409, "already resolved", cid)
    evidence = worm.store_evidence(
        subject=cid, kind="corrected-record",
        payload={"case": case["title"], "defect_id": case["defect_id"],
                 "correction": body.correction, "note": body.note,
                 "resolved_by": principal.sub})
    with core.db() as conn:
        conn.execute(
            "UPDATE cases SET status='resolved', updated_at=?, worm_evidence_id=? "
            "WHERE id=?", (core.now(), evidence["id"], cid))
        if case["defect_id"]:
            conn.execute("UPDATE defects SET status='corrected' WHERE id=?",
                         (case["defect_id"],))
    return {"case": _case(cid), "evidence": evidence}


# ------------------------------------------------------------- ETL
@app.post("/v1/etl/extract", status_code=201)
def etl_extract(principal=AuthDep):
    """Controlled ETL extract: legacy_records -> staging (validated shape)."""
    recs = _all_records()
    batch_id = f"etl-{uuid.uuid4().hex[:12]}"
    staged = 0
    with core.db() as conn:
        for r in recs:
            if not r.record_id or not core.normalise_tin(r.tin):
                continue  # control: reject rows without keys
            conn.execute("INSERT OR REPLACE INTO etl_staging VALUES (?,?,?)",
                         (r.record_id, json.dumps(r.__dict__), batch_id))
            staged += 1
    return {"batch_id": batch_id, "staged": staged, "rejected": len(recs) - staged}


@app.post("/v1/etl/load")
def etl_load(batch_id: str = "", principal=AuthDep):
    """Controlled ETL load: staging -> clean store (latest batch by default)."""
    with core.db() as conn:
        if not batch_id:
            row = conn.execute(
                "SELECT batch_id FROM etl_staging ORDER BY rowid DESC LIMIT 1"
            ).fetchone()
            if row is None:
                return problem(404, "no staging batch", "run /v1/etl/extract first")
            batch_id = row["batch_id"]
        rows = conn.execute(
            "SELECT * FROM etl_staging WHERE batch_id=?", (batch_id,)).fetchall()
        for row in rows:
            conn.execute("INSERT OR REPLACE INTO etl_clean VALUES (?,?,?)",
                         (row["record_id"], row["payload"], batch_id))
    return {"batch_id": batch_id, "loaded": len(rows)}


@app.get("/v1/evidence/{evidence_id}/verify")
def evidence_verify(evidence_id: str, principal=AuthDep):
    return worm.verify_evidence(evidence_id)
