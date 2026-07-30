"""TP/CbCR service (SPEC 3 T8): entity/transaction graph ingest, OECD CbCR
XML generation, master/local file assembly, connected-party interest
deductibility, rp-tp-2018 with per-tenant pack pins, FX table.

REST:
  GET  /healthz, /readyz
  POST /v1/graph/entities, GET /v1/graph/entities
  POST /v1/graph/transactions, GET /v1/graph/transactions
  POST /v1/cbcr/generate                 build CbCR XML (JSON or ?format=xml)
  POST /v1/docs/master-file              assemble master file (JSON or ?format=html)
  POST /v1/docs/local-file               assemble local file (JSON or ?format=html)
  POST /v1/interest/deductibility        connected-party interest calculator
  POST /v1/tp/evaluate                   rp-tp-2018 evaluation (tenant pin)
  GET  /v1/packs/pins, PUT /v1/packs/pin/{tenant_id}
  GET  /v1/fx, POST /v1/fx/rates, POST /v1/fx/convert
"""

from __future__ import annotations

from typing import Optional

from fastapi import FastAPI, HTTPException, Query, Request
from fastapi.responses import HTMLResponse, Response
from pydantic import BaseModel, Field

from meridian_py.dev_jwt import AuthDep, problem
from meridian_py.rulepack import PackRegistry

from . import cbcr, graph, interest, tpdocs

SERVICE = "tp-cbcr"
VERSION = "1.0.0"

app = FastAPI(title="Meridian TP/CbCR Service", version=VERSION)


@app.exception_handler(HTTPException)
async def http_exc_handler(_: Request, exc: HTTPException):
    return problem(exc.status_code, str(exc.detail), "")


@app.get("/healthz")
def healthz():
    return {"status": "ok", "service": SERVICE, "version": VERSION}


@app.get("/readyz")
def readyz():
    pack = PackRegistry().load("rp-tp-2018")
    return {"status": "ready", "baseline_pack": pack.ref}


# ------------------------------------------------------------- graph
class EntityIn(BaseModel):
    tin: str
    name: str
    jurisdiction: str = "NG"
    role: str = "subsidiary"   # ultimate_parent | intermediate | subsidiary
    entity_type: str = "company"
    biz_activity: str = "CBC501"


@app.post("/v1/graph/entities", status_code=201)
def add_entity(body: EntityIn, principal=AuthDep):
    return graph.add_entity(body.model_dump(), principal.tenant_id)


@app.get("/v1/graph/entities")
def list_entities(principal=AuthDep):
    ents = graph.list_entities(principal.tenant_id)
    return {"count": len(ents), "entities": ents}


class TxIn(BaseModel):
    from_tin: str
    to_tin: str
    tx_type: str = "service"   # service|goods|loan|interest|royalty|cost-sharing
    amount_kobo: int = Field(..., ge=0)
    currency: str = "NGN"


@app.post("/v1/graph/transactions", status_code=201)
def add_tx(body: TxIn, principal=AuthDep):
    return graph.add_transaction(body.model_dump(), principal.tenant_id)


@app.get("/v1/graph/transactions")
def list_tx(principal=AuthDep):
    txs = graph.list_transactions(principal.tenant_id)
    return {"count": len(txs), "transactions": txs,
            "controlled_total_kobo": graph.controlled_transactions_total()}


# ------------------------------------------------------------- CbCR
@app.post("/v1/cbcr/generate", status_code=201)
def generate_cbcr(body: dict, format: str = Query("json"), principal=AuthDep):
    if not body.get("reporting_entity") or not body.get("reporting_period"):
        return problem(422, "bad request",
                       "reporting_entity and reporting_period are required")
    # CbCR obligation check against rp-tp-2018
    revenue = sum(int(j.get("revenue_unrelated_kobo", 0))
                  + int(j.get("revenue_related_kobo", 0))
                  for j in body.get("jurisdictions", []))
    check = interest.evaluate_tp_pack(
        principal.tenant_id, {"group_revenue_kobo": revenue})
    xml_text = cbcr.build_cbcr_xml(body)
    report_id = graph.save_report("cbcr", {"xml_len": len(xml_text),
                                           "period": body["reporting_period"]})
    if format == "xml":
        return Response(xml_text, media_type="application/xml")
    return {"report_id": report_id, "cbcr_required": bool(
                check["decision"].get("cbcr_required")),
            "pack": check.get("pack_ref"), "xml": xml_text}


# ------------------------------------------------------------- docs
@app.post("/v1/docs/master-file", status_code=201)
def master_file(body: dict, format: str = Query("json"), principal=AuthDep):
    entities = graph.list_entities()
    transactions = graph.list_transactions()
    doc = tpdocs.assemble_master_file(body.get("group", {}), entities,
                                      transactions)
    graph.save_report("master_file", {"sections": list(doc["sections"])})
    if format == "html":
        return HTMLResponse(tpdocs.render_html(doc))
    return doc


@app.post("/v1/docs/local-file", status_code=201)
def local_file(body: dict, format: str = Query("json"), principal=AuthDep):
    entity = body.get("entity") or {}
    if not entity.get("tin"):
        return problem(422, "bad request", "entity.tin is required")
    doc = tpdocs.assemble_local_file(entity, graph.list_transactions(),
                                     body.get("financials", {}))
    graph.save_report("local_file", {"entity": entity.get("tin")})
    if format == "html":
        return HTMLResponse(tpdocs.render_html(doc))
    return doc


# ------------------------------------------------------------- interest
@app.post("/v1/interest/deductibility")
def interest_ded(body: dict, principal=AuthDep):
    body = dict(body or {})
    body.setdefault("tenant_id", principal.tenant_id)
    return interest.interest_deductibility(body)


# ------------------------------------------------------------- packs
@app.post("/v1/tp/evaluate")
def tp_evaluate(body: dict, principal=AuthDep):
    ctx = body.get("context", {})
    return interest.evaluate_tp_pack(principal.tenant_id, ctx)


class PinIn(BaseModel):
    pack_id: str = "rp-tp-2018"
    version: str


@app.put("/v1/packs/pin/{tenant_id}")
def put_pin(tenant_id: str, body: PinIn, principal=AuthDep):
    # validate the version exists before pinning
    try:
        PackRegistry().load(body.pack_id, body.version)
    except FileNotFoundError as exc:
        return problem(404, "pack version not found", str(exc))
    return graph.pin_pack(tenant_id, body.pack_id, body.version)


@app.get("/v1/packs/pins")
def get_pins(principal=AuthDep):
    return {"pins": graph.list_pins()}


# ------------------------------------------------------------- FX
@app.get("/v1/fx")
def fx(principal=AuthDep):
    return {"base": "NGN", "rates": graph.fx_table()}


class FxRateIn(BaseModel):
    currency: str
    per_ngn: float = Field(..., gt=0)
    as_of: str


@app.post("/v1/fx/rates", status_code=201)
def fx_rate(body: FxRateIn, principal=AuthDep):
    return graph.upsert_fx(body.currency, body.per_ngn, body.as_of)


@app.post("/v1/fx/convert")
def fx_convert_ep(body: dict, principal=AuthDep):
    try:
        out = graph.fx_convert(int(body["amount_minor"]),
                               body["from"], body["to"])
    except (KeyError, ValueError) as exc:
        return problem(422, "fx error", str(exc))
    return {"amount_minor": int(body["amount_minor"]), "from": body["from"],
            "to": body["to"], "converted_minor": out}
