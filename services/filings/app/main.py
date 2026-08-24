"""filings service — periodic filing layer: VAT Form 002, PAYE schedules +
Form H1, CIT computation, assessment & objection lifecycle. See README.md
for REAL/SIM honesty tags per module."""
from __future__ import annotations

import os
from datetime import date

from fastapi import Depends, FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse
from pydantic import BaseModel

from . import assessment, authz, cit, paye, vat
from meridian_py.dev_jwt import Principal

app = FastAPI(title="filings", version="1.0.0")

# B2 #1: fail closed at boot when prod/keycloak auth is misconfigured
authz.validate_auth_config()

vat_store = vat.VatReturnStore()
paye_store = paye.PayeReturnStore()
asm_store = assessment.AssessmentStore()


@app.exception_handler(HTTPException)
async def http_exc(request: Request, exc: HTTPException):
    return JSONResponse(status_code=exc.status_code,
                        media_type="application/problem+json",
                        content={"type": f"https://meridian.ng/problems/{exc.status_code}",
                                 "title": str(exc.detail), "status": exc.status_code})


def _err(exc: ValueError, status: int = 422):
    raise HTTPException(status_code=status, detail=str(exc))


@app.get("/healthz")
def healthz():
    return {"status": "ok", "service": "filings",
            "profile": "prod" if os.environ.get("AUTH_MODE") == "keycloak" else "dev"}


# ---------- F1 VAT Form 002 ----------

class VatFileIn(BaseModel):
    tin: str
    period: str
    idempotency_key: str
    invoices: list[dict] = []
    sales_schedule: dict | None = None
    purchases: list[dict] = []
    adjustments: list[dict] = []
    exempt_input_share_bps: int = 0
    amendment_of: str | None = None


@app.post("/v1/filings/vat", status_code=201)
def file_vat(body: VatFileIn, principal: Principal = Depends(authz.require_filer)):
    authz.require_tin_scope(principal, body.tin)
    try:
        ret = vat.build_return(body.tin, body.period, body.invoices,
                               body.sales_schedule, body.purchases,
                               body.adjustments, body.exempt_input_share_bps)
        rec, created = vat_store.file(ret, body.idempotency_key, body.amendment_of)
    except ValueError as e:
        _err(e)
    return JSONResponse(status_code=201 if created else 200, content=rec)


@app.get("/v1/filings/vat/{tin}/{period}")
def get_vat(tin: str, period: str,
            principal: Principal = Depends(authz.require_reader)):
    authz.require_tin_scope(principal, tin)
    rec = vat_store.get(tin, period)
    if rec is None:
        raise HTTPException(status_code=404, detail="no VAT return for tin/period")
    return rec


# ---------- F2 PAYE ----------

class PayeFileIn(BaseModel):
    employer_tin: str
    period: str
    idempotency_key: str
    employees: list[dict]


@app.post("/v1/filings/paye", status_code=201)
def file_paye(body: PayeFileIn,
              principal: Principal = Depends(authz.require_filer)):
    authz.require_tin_scope(principal, body.employer_tin)
    try:
        sched = paye.build_monthly_schedule(body.employer_tin, body.period,
                                            body.employees)
        rec, created = paye_store.file(sched, body.idempotency_key)
    except ValueError as e:
        _err(e)
    return JSONResponse(status_code=201 if created else 200, content=rec)


@app.post("/v1/filings/paye/h1")
def build_h1(employer_tin: str, year: int,
             principal: Principal = Depends(authz.require_filer)):
    authz.require_tin_scope(principal, employer_tin)
    try:
        return paye.build_form_h1(employer_tin, year,
                                  paye_store.for_year(employer_tin, year))
    except ValueError as e:
        _err(e)


# ---------- F3 CIT ----------

class CitComputeIn(BaseModel):
    tin: str
    fye: date
    assessable_profit_kobo: int
    turnover_kobo: int
    total_fixed_assets_kobo: int
    assets: list[dict] = []
    losses: list[dict] = []


@app.post("/v1/filings/cit/compute")
def compute_cit(body: CitComputeIn,
                principal: Principal = Depends(authz.require_filer)):
    authz.require_tin_scope(principal, body.tin)
    try:
        return cit.compute_return(body.tin, body.fye,
                                  body.assessable_profit_kobo,
                                  body.turnover_kobo,
                                  body.total_fixed_assets_kobo,
                                  body.assets, body.losses)
    except ValueError as e:
        _err(e)


# ---------- F4 assessments & objections ----------

class AssessmentIssueIn(BaseModel):
    tin: str
    tax_type: str
    period: str
    kind: str
    amount_kobo: int
    grounds: str
    served_via: str
    served_at: date


@app.post("/v1/assessments", status_code=201)
def issue_assessment(body: AssessmentIssueIn,
                     principal: Principal = Depends(authz.require_officer)):
    try:
        return asm_store.issue(body.tin, body.tax_type, body.period, body.kind,
                               body.amount_kobo, body.grounds,
                               body.served_via, body.served_at)
    except ValueError as e:
        _err(e)


@app.get("/v1/assessments/tat-referrals")
def tat_referrals(principal: Principal = Depends(authz.require_officer)):
    return {"referrals": asm_store.tat_referrals()}


@app.get("/v1/assessments/{assessment_id}")
def get_assessment(assessment_id: str,
                   principal: Principal = Depends(authz.require_reader)):
    rec = asm_store.get(assessment_id)
    if rec is None:
        raise HTTPException(status_code=404, detail="unknown assessment")
    authz.require_tin_scope(principal, rec["tin"])
    return rec


class ObjectionIn(BaseModel):
    grounds: str
    admitted_amount_kobo: int
    paid_admitted_kobo: int = 0
    filed_at: date


@app.post("/v1/assessments/{assessment_id}/objections", status_code=201)
def file_objection(assessment_id: str, body: ObjectionIn,
                   principal: Principal = Depends(authz.require_filer)):
    rec = asm_store.get(assessment_id)
    if rec is None:
        raise HTTPException(status_code=404, detail="unknown assessment")
    authz.require_tin_scope(principal, rec["tin"])
    try:
        return asm_store.object(assessment_id, body.grounds,
                                body.admitted_amount_kobo,
                                body.paid_admitted_kobo, body.filed_at)
    except ValueError as e:
        _err(e)


class DecisionIn(BaseModel):
    outcome: str
    decided_at: date
    revised_amount_kobo: int | None = None


@app.post("/v1/objections/{objection_id}/decision")
def decide_objection(objection_id: str, body: DecisionIn,
                     principal: Principal = Depends(authz.require_officer)):
    try:
        return asm_store.decide(objection_id, body.outcome, body.decided_at,
                                body.revised_amount_kobo)
    except ValueError as e:
        _err(e)


@app.post("/v1/assessments/tick")
def tick(today: date, principal: Principal = Depends(authz.require_officer)):
    return {"events": asm_store.tick(today)}
