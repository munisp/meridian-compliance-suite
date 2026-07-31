"""insights service — Part B innovations I8-I12, I14. See README.md for
REAL/SIMULATED honesty tags per module."""
from __future__ import annotations

import os
from datetime import date

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel

from . import benchmarks, circularity, explain, fx, penalties, reminders

app = FastAPI(title="insights", version="1.0.0")


@app.get("/healthz")
def healthz():
    return {"status": "ok", "service": "insights",
            "profile": "prod" if os.environ.get("AUTH_MODE") == "keycloak" else "dev"}


# I8 circularity
class CircularityIn(BaseModel):
    invoices: list[dict]
    max_len: int = 5
    min_vat_kobo: int = 1


@app.post("/v1/insights/circularity")
def post_circularity(body: CircularityIn):
    g = circularity.build_graph(body.invoices)
    cycles = circularity.find_cycles(g, body.max_len, body.min_vat_kobo)
    return {"backend": "in-memory", "nodes": len(g.nodes()),
            "cycles": [c.as_dict() for c in cycles]}


# I9 benchmarks
class BenchmarkIn(BaseModel):
    records: list[dict]
    sigma: float = 2.0


@app.post("/v1/insights/benchmarks")
def post_benchmarks(body: BenchmarkIn):
    recs = [benchmarks.TaxpayerPeriod(r["tin"], r["sector"],
                                      int(r["turnover_kobo"]), int(r["tax_paid_kobo"]))
            for r in body.records]
    return benchmarks.sector_report(recs, body.sigma)


# I10 penalties
class PenaltyIn(BaseModel):
    tax_type: str
    due_date: str
    filed_date: str | None = None
    paid_date: str | None = None
    tax_kobo: int = 0


@app.post("/v1/insights/penalties")
def post_penalties(body: PenaltyIn):
    try:
        res = penalties.compute(
            body.tax_type, date.fromisoformat(body.due_date),
            date.fromisoformat(body.filed_date) if body.filed_date else None,
            date.fromisoformat(body.paid_date) if body.paid_date else None,
            body.tax_kobo)
    except (KeyError, ValueError) as exc:
        raise HTTPException(422, str(exc))
    return res.as_dict()


# I11 reminders
class ReminderIn(BaseModel):
    tenant_id: str
    tax: str
    period: str
    year_end: str | None = None
    history: list[dict] = []


@app.post("/v1/insights/reminders")
def post_reminders(body: ReminderIn):
    r = reminders.schedule(
        body.tenant_id, body.tax, date.fromisoformat(body.period),
        body.history, date.fromisoformat(body.year_end) if body.year_end else None)
    if not r:
        raise HTTPException(422, f"no calendar rule for tax {body.tax}")
    return {"reminder": r.__dict__, "event": r.event()}


# I12 explainability
class ExplainIn(BaseModel):
    invoice_id: str
    trace: list[dict]


@app.post("/v1/insights/explain")
def post_explain(body: ExplainIn):
    card = explain.build_card(body.invoice_id, body.trace)
    return card.as_dict()


# I14 fx
class FXIn(BaseModel):
    amount_minor: int
    currency: str
    date: str
    invoice_id: str = ""


_fx = fx.FXService()


@app.post("/v1/insights/fx/convert")
def post_fx(body: FXIn):
    try:
        return _fx.convert_to_ngn_kobo(body.amount_minor, body.currency,
                                       date.fromisoformat(body.date), body.invoice_id)
    except KeyError as exc:
        raise HTTPException(422, str(exc))


@app.get("/v1/insights/fx/audit")
def fx_audit():
    return {"entries": _fx.audit[-100:], "count": len(_fx.audit)}
