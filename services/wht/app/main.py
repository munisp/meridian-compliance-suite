"""WHT service (SPEC 3 T7) — WHT Regulations 2024 engine, vendor TIN
validation, credit ledger, remittance files, wf-wht-remit-schedule.

REST:
  GET  /healthz, /readyz
  POST /v1/wht/evaluate                     evaluate a deduction (rp-wht-2024)
  POST /v1/wht/deductions                   record an evaluated deduction
  GET  /v1/wht/deductions                   list deductions
  POST /v1/wht/remit-file                   generate remittance CSV + XML
  GET  /v1/wht/credits/{vendor_tin}         vendor credit ledger balance
  POST /v1/wht/credits/{vendor_tin}/apply   apply (use) credit
  POST /v1/wht/vendors/verify-tin           vendor-master TIN validation
  POST /v1/wht/workflows/remit-schedule/run run wf-wht-remit-schedule
  GET  /v1/wht/workflows                    workflow run history
"""

from __future__ import annotations

import uuid
from typing import Optional

from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import Response
from pydantic import BaseModel, Field

from meridian_py.dev_jwt import AuthDep, problem, validate_auth_config
from meridian_py.rulepack import PackRegistry

from . import certs, db, engine as wht_engine, workflow

# Fail closed at startup when AUTH_MODE=keycloak is missing OIDC config.
validate_auth_config()

SERVICE = "wht"
VERSION = "1.0.0"

app = FastAPI(title="Meridian WHT 2024 Service", version=VERSION)


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
    pack = PackRegistry().load("rp-wht-2024")
    return {"status": "ready", "pack": pack.ref,
            "subject_to_regazette": pack.subject_to_regazette}


class EvaluateIn(BaseModel):
    payment_type: str = Field(..., description="canonical rp-wht-2024 vocabulary: dividend|interest|rent|royalty|supply_of_goods_materials|construction|consultancy|professional|technical|management|services|commission|directors_fees (legacy aliases goods/contract/service_fee/director_fee accepted)")
    beneficiary: str = Field("company", description="company|individual")
    amount_kobo: int = Field(..., gt=0)
    includes_vat: bool = Field(False, description="the amount is VAT-inclusive; WHT base is net of VAT (pack rule wht.base.vat-exclusive)")
    vat_kobo: int = Field(0, ge=0, description="VAT component of amount_kobo when includes_vat is true")
    supplier_monthly_turnover_kobo: Optional[int] = Field(
        None, description="LEGACY supplier-side fact (pre-audit pack); the carve-out now keys on the PAYER")
    supplier_size: str = Field("", description="LEGACY supplier-side fact (pre-audit pack)")
    payer_size: str = Field("", description="small|medium|large — small-company carve-out requires 'small' (payer-side, Reg 4)")
    payer_is_small_company: bool = Field(False, description="alias for payer_size=small")
    payer_annual_turnover_kobo: Optional[int] = Field(
        None, description="payer's annual turnover (small company <= N25m p.a. = 2,500,000,000 kobo)")
    transaction_month_value_kobo: Optional[int] = Field(
        None, description="transaction value within the calendar month (carve-out cap N2m = 200,000,000 kobo)")
    beneficiary_residence: str = Field("", description="resident|non_resident")
    source: str = Field("", description="winnings source: lottery|gaming|reality_show")
    construction_type: str = Field("", description="roads|bridges|buildings|power_plants|other")
    tax: str = Field("", description="tax discriminator for multi-tax packs (default WHT)")
    supplier_tin: str = ""
    nin: str = ""
    payment_date: str = ""
    settlement_date: str = ""
    date: str = Field("", description="explicit transaction date fallback (ISO YYYY-MM-DD)")
    via_direct_debit: bool = False
    via_broker: bool = False
    supplier_is_manufacturer: bool = False
    goods_imported: bool = False
    vendor_name: str = ""
    tenant_id: str = ""
    idempotency_key: str = ""  # dedup retried POSTs (F3b)
    record: bool = False  # also persist as a ledger deduction


@app.post("/v1/wht/evaluate")
def evaluate(body: EvaluateIn, principal=AuthDep):
    try:
        result = wht_engine.evaluate_wht(body.model_dump())
    except ValueError as exc:
        return problem(422, "evaluation failed", str(exc))
    if body.record:
        try:
            deduction = _persist_deduction(body, result)
        except IdempotencyConflict as exc:
            return problem(409, "Idempotency conflict", str(exc))
        result["deduction_id"] = deduction
    return result


class IdempotencyConflict(Exception):
    """B3 #20: idempotency key replayed with a different payload."""


def _deduction_payload_hash(body: "EvaluateIn", result: dict) -> str:
    import hashlib
    import json
    body_hash = json.dumps(
        {"supplier_tin": body.supplier_tin, "vendor_name": body.vendor_name,
         "payment_type": body.payment_type, "beneficiary": body.beneficiary,
         "amount_kobo": body.amount_kobo, "tenant_id": body.tenant_id,
         "wht_kobo": result["wht_kobo"], "rate_bps": result["rate_bps"],
         "outcome": result["outcome"]},
        sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(body_hash).hexdigest()


def _persist_deduction(body: EvaluateIn, result: dict) -> str:
    # F3b: caller idempotency key -> deterministic deduction id; a retried
    # POST replays the original deduction instead of double-counting it
    # into the next remittance run.
    # B3 #20: the binding is payload-bound (conflict on key reuse with a
    # different payload) and concurrent same-key inserts replay instead of
    # surfacing a 500 IntegrityError.
    phash = ""
    if getattr(body, "idempotency_key", ""):
        import hashlib
        did = "ded-" + hashlib.sha256(
            f"idem:{body.idempotency_key}".encode()).hexdigest()[:12]
        phash = _deduction_payload_hash(body, result)
        with db.session() as sess:
            existing = sess.get(db.Deduction, did)
            if existing is not None:
                if existing.payload_hash and existing.payload_hash != phash:
                    raise IdempotencyConflict(
                        "idempotency_key reused with a different payload")
                return did  # idempotent replay
    else:
        did = f"ded-{uuid.uuid4().hex[:12]}"
    date = result.get("deduction_date") or db.now()[:10]
    from sqlalchemy.exc import IntegrityError
    sess = db.session()
    try:
        deduction = db.Deduction(
            id=did, tenant_id=body.tenant_id or principal_tenant(),
            vendor_tin=body.supplier_tin, vendor_name=body.vendor_name,
            payment_type=body.payment_type, beneficiary=body.beneficiary,
            amount_kobo=body.amount_kobo, rate_bps=result["rate_bps"],
            wht_kobo=result["wht_kobo"], outcome=result["outcome"],
            deduction_trigger=result["deduction_trigger"],
            deduction_date=date, period=date[:7], payload_hash=phash)
        sess.add(deduction)
        sess.flush()
        # WHT Regs 2024 reg. 7 (audit R4 S1a#10): issue the signed vendor
        # credit certificate for every deduction.
        certs.issue_certificate(sess, deduction)
        sess.commit()
    except IntegrityError:
        # B3 #20: lost the same-key race — the other in-flight request
        # committed first. Replay its row (or conflict on payload mismatch)
        # instead of surfacing a 500.
        sess.rollback()
        existing = sess.get(db.Deduction, did)
        if existing is None:
            raise
        if existing.payload_hash and phash and existing.payload_hash != phash:
            raise IdempotencyConflict(
                "idempotency_key reused with a different payload")
        return did
    finally:
        sess.close()
    return did


def principal_tenant() -> str:
    return ""


@app.post("/v1/wht/deductions", status_code=201)
def create_deduction(body: EvaluateIn, principal=AuthDep):
    try:
        result = wht_engine.evaluate_wht(body.model_dump())
    except ValueError as exc:
        return problem(422, "evaluation failed", str(exc))
    try:
        did = _persist_deduction(body, result)
    except IdempotencyConflict as exc:
        return problem(409, "Idempotency conflict", str(exc))
    return {"deduction_id": did, "evaluation": result}


@app.get("/v1/wht/deductions")
def list_deductions(period: Optional[str] = None,
                    remitted: Optional[bool] = None,
                    limit: int = 500,
                    offset: int = 0,
                    principal=AuthDep):
    """Paginated deduction listing (PERF: was unbounded — 294 ms / 1.7 MB at
    5.1k rows, growing linearly). Backward compatible: the default page of
    500 preserves small-ledger clients; `count` still counts the returned
    page and `total` reports the full filtered cardinality."""
    from sqlalchemy import func, select
    limit = max(1, min(limit, 1000))  # clamp: sane default, hard max
    offset = max(0, offset)
    with db.session() as sess:
        q = select(db.Deduction)
        if period:
            q = q.where(db.Deduction.period == period)
        if remitted is not None:
            q = q.where(db.Deduction.remitted.is_(remitted))
        total = int(sess.execute(
            select(func.count()).select_from(q.subquery())).scalar_one())
        rows = list(sess.execute(
            q.order_by(db.Deduction.id).limit(limit).offset(offset)).scalars())
    return {"count": len(rows), "total": total, "limit": limit,
            "offset": offset, "deductions": [
        {c.name: getattr(r, c.name) for c in db.Deduction.__table__.columns}
        for r in rows]}


class RemitFileIn(BaseModel):
    period: str = ""
    tenant_id: str = ""


@app.post("/v1/wht/remit-file", status_code=201)
def remit_file(body: RemitFileIn, principal=AuthDep):
    """Generate the remittance file (CSV + XML) via wf-wht-remit-schedule."""
    run = workflow.wf_wht_remit_schedule(period=body.period,
                                         tenant_id=body.tenant_id)
    if run.status != "completed":
        return problem(422, "workflow failed", run.result.get("error", ""))
    return {"run_id": run.id, **{k: v for k, v in run.result.items()
                                 if k not in ("csv", "xml")},
            "files": {"csv": run.result["csv"], "xml": run.result["xml"]}}


@app.get("/v1/wht/credits/{vendor_tin}")
def get_credits(vendor_tin: str, principal=AuthDep):
    with db.session() as sess:
        entries = db.vendor_credits(sess, vendor_tin)
        balance = db.credit_balance(sess, vendor_tin)
    return {"vendor_tin": vendor_tin, "balance_kobo": balance,
            "entries": [{c.name: getattr(e, c.name)
                         for c in db.Credit.__table__.columns}
                        for e in entries]}


class ApplyCreditIn(BaseModel):
    amount_kobo: int = Field(..., gt=0)
    includes_vat: bool = Field(False, description="the amount is VAT-inclusive; WHT base is net of VAT (pack rule wht.base.vat-exclusive)")
    vat_kobo: int = Field(0, ge=0, description="VAT component of amount_kobo when includes_vat is true")
    note: str = ""
    idempotency_key: str = ""  # B3 #10: dedup retried applies


@app.post("/v1/wht/credits/{vendor_tin}/apply", status_code=201)
def apply_credit(vendor_tin: str, body: ApplyCreditIn, principal=AuthDep):
    # B3 #10: the old SUM-then-check-then-insert sequence raced — two
    # concurrent applies could both pass the balance check and overdraw
    # the credit ledger. The guard is a single atomic statement: the
    # INSERT only happens WHERE the current balance covers the amount, so
    # the check and the debit commit as one database operation.
    # R3 verifier: that statement is atomic on SQLite but races on Postgres
    # READ COMMITTED (both concurrent statements snapshot pre-race -> both
    # insert -> overdraw). On Postgres the whole transaction is therefore
    # serialized per credit account with pg_advisory_xact_lock BEFORE the
    # balance-checking insert (see db.acquire_credit_lock).
    import hashlib
    from sqlalchemy import text
    from sqlalchemy.exc import IntegrityError
    cid = ("cr-" + hashlib.sha256(
        f"idem:{body.idempotency_key}".encode()).hexdigest()[:12]
        if body.idempotency_key else f"cr-{uuid.uuid4().hex[:12]}")
    with db.session() as sess:
        db.acquire_credit_lock(sess, principal_tenant(), vendor_tin)
        if body.idempotency_key:
            existing = sess.get(db.Credit, cid)
            if existing is not None:
                if (existing.vendor_tin != vendor_tin
                        or existing.credit_kobo != -body.amount_kobo):
                    return problem(409, "Idempotency conflict",
                                   "idempotency_key reused with a different payload")
                return {"credit_id": cid, "applied_kobo": body.amount_kobo,
                        "balance_kobo": db.credit_balance(sess, vendor_tin),
                        "replayed": True}
        stmt = text(
            "INSERT INTO wht_credits"
            " (id, tenant_id, vendor_tin, credit_kobo, source, period, note, created_at)"
            " SELECT :id, :tenant, :vtin, :amt, 'application', '', :note, :now"
            " WHERE (SELECT COALESCE(SUM(credit_kobo), 0) FROM wht_credits"
            "        WHERE vendor_tin = :vtin) >= :need")
        try:
            res = sess.execute(stmt, {
                "id": cid, "tenant": principal_tenant(), "vtin": vendor_tin,
                "amt": -body.amount_kobo, "note": body.note,
                "now": db.now(), "need": body.amount_kobo})
            sess.commit()
        except IntegrityError:
            # concurrent same-key apply committed first — replay it
            sess.rollback()
            existing = sess.get(db.Credit, cid)
            if existing is None:
                raise
            if (existing.vendor_tin != vendor_tin
                    or existing.credit_kobo != -body.amount_kobo):
                return problem(409, "Idempotency conflict",
                               "idempotency_key reused with a different payload")
            return {"credit_id": cid, "applied_kobo": body.amount_kobo,
                    "balance_kobo": db.credit_balance(sess, vendor_tin),
                    "replayed": True}
        if res.rowcount == 0:
            balance = db.credit_balance(sess, vendor_tin)
            return problem(422, "insufficient credit",
                           f"balance {balance} kobo < requested {body.amount_kobo}")
        new_balance = db.credit_balance(sess, vendor_tin)
    return {"credit_id": cid, "applied_kobo": body.amount_kobo,
            "balance_kobo": new_balance}


@app.get("/v1/wht/certificates/{vendor_tin}")
def list_certificates(vendor_tin: str, principal=AuthDep):
    """Vendor-retrievable WHT credit certificates (reg. 7). The vendor's own
    certificates are listed; cross-vendor listing is refused."""
    from sqlalchemy import select
    with db.session() as sess:
        rows = list(sess.execute(
            select(db.Certificate).where(
                db.Certificate.vendor_tin == vendor_tin)).scalars())
        return {"vendor_tin": vendor_tin, "count": len(rows),
                "certificates": [certs.certificate_view(c) for c in rows]}


@app.get("/v1/wht/certificates/{vendor_tin}/{certificate_id}")
def get_certificate(vendor_tin: str, certificate_id: str, verify: bool = False,
                    principal=AuthDep):
    from sqlalchemy import select
    with db.session() as sess:
        cert = sess.get(db.Certificate, certificate_id)
        if cert is None or cert.vendor_tin != vendor_tin:
            raise HTTPException(404, f"certificate {certificate_id} not found")
        view = certs.certificate_view(cert)
        if verify:
            view["signature_valid"] = certs.verify_certificate(cert)
        return view


class RefundIn(BaseModel):
    deduction_id: str
    corrected: dict = Field(..., description="corrected evaluate request (engine re-evaluates)")
    idempotency_key: str = ""


@app.post("/v1/wht/refunds", status_code=201)
def create_refund(body: RefundIn, principal=AuthDep):
    """Over-deduction refund: the engine re-evaluates the deduction with the
    corrected facts; the excess WHT flows into the vendor's credit ledger
    (existing refund machinery, applyable via /v1/wht/credits/{tin}/apply)."""
    with db.session() as sess:
        deduction = sess.get(db.Deduction, body.deduction_id)
        if deduction is None:
            raise HTTPException(404, f"deduction {body.deduction_id} not found")
        try:
            refund = certs.process_refund(
                sess, deduction, body.corrected, actor="api",
                idempotency_key=body.idempotency_key)
            sess.commit()
        except ValueError as exc:
            sess.rollback()
            return problem(422, "refund refused", str(exc))
        return {c.name: getattr(refund, c.name)
                for c in db.Refund.__table__.columns}


@app.get("/v1/wht/refunds")
def list_refunds(vendor_tin: str = "", principal=AuthDep):
    from sqlalchemy import select
    with db.session() as sess:
        q = select(db.Refund)
        if vendor_tin:
            q = q.where(db.Refund.vendor_tin == vendor_tin)
        rows = list(sess.execute(q).scalars())
        return {"count": len(rows), "refunds": [
            {c.name: getattr(r, c.name) for c in db.Refund.__table__.columns}
            for r in rows]}


@app.post("/v1/wht/vendors/verify-tin")
def verify_tin(body: dict, principal=AuthDep):
    tin = (body or {}).get("tin", "")
    check = wht_engine.validate_tin(tin)
    return check.__dict__


@app.post("/v1/wht/workflows/remit-schedule/run")
def run_workflow(body: RemitFileIn, principal=AuthDep):
    run = workflow.wf_wht_remit_schedule(period=body.period,
                                         tenant_id=body.tenant_id)
    return {"run": {"id": run.id, "name": run.name, "status": run.status,
                    "steps": [s.__dict__ for s in run.steps],
                    "result": {k: v for k, v in run.result.items()
                               if k not in ("csv", "xml")}}}


@app.get("/v1/wht/workflows")
def list_workflows(principal=AuthDep):
    return {"registered": ["wf-wht-remit-schedule"],
            "runs": [{"id": r.id, "name": r.name, "status": r.status,
                      "started_at": r.started_at,
                      "finished_at": r.finished_at}
                     for r in workflow.runs()]}


@app.get("/v1/wht/pack")
def pack_info(principal=AuthDep):
    pack = PackRegistry().load("rp-wht-2024")
    return {"id": pack.id, "version": pack.version, "ref": pack.ref,
            "status": pack.status,
            "subject_to_regazette": pack.subject_to_regazette,
            "provenance": pack.provenance, "rules": len(pack.rules)}


@app.get("/v1/wht/remit-file/{batch_id}", response_class=Response)
def download_remit_file(batch_id: str, fmt: str = "csv", principal=AuthDep):
    from sqlalchemy import select
    with db.session() as sess:
        rows = list(sess.execute(
            select(db.Deduction).where(db.Deduction.remit_batch == batch_id)
        ).scalars())
    if not rows:
        raise HTTPException(404, f"batch {batch_id} not found")
    deductions = [{c.name: getattr(r, c.name)
                   for c in db.Deduction.__table__.columns} for r in rows]
    from . import remit
    if fmt == "xml":
        return Response(remit.remittance_xml(batch_id, deductions,
                                             deductions[0]["period"]),
                        media_type="application/xml")
    return Response(remit.remittance_csv(batch_id, deductions),
                    media_type="text/csv")
