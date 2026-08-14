"""str-filing service — Suspicious Transaction Report (STR) / AML
regulatory-filing pipeline for the Meridian NRS platform.

Closes assurance waveC gaps C4 (STR filing queue), C7 (durable STR DLQ),
C8 (manual requeue) and C10 (STR-specific audit trail). Modelled on the
proven Odoo nrs.submission_log retry/requeue pattern.

Intake: REST POST /v1/str and Kafka topic ``nrs.aml.str.created`` (PEP/EDD
and sanctions-hit risk events from kyc-engine). Queue: durable Postgres
table str_filings. Submission: NFIU HTTP adapter (REAL, prod default) with
a SIM adapter behind the same interface for dev/test (tagged SIM, refused
in prod profile). Retries: exponential backoff, dead-letter after
max_attempts, RBAC-gated manual requeue (Permify checkRel pattern from
case-mgmt). Audit: WORM-style record per state transition via the platform
audit-evidence API (local hash-chained fallback in dev).
"""
from __future__ import annotations

import hashlib
import json
import logging
import os
import threading

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, PlainTextResponse
from pydantic import BaseModel, Field
from prometheus_client import generate_latest

from . import authz, bus, db
from .audit import audit_from_env
from .nfiu import SimNFIUClient, adapter_from_env
from .worker import FilingWorker, Metrics, start_background

log = logging.getLogger("str-filing")

DATA_DIR = os.environ.get("DATA_DIR", "/tmp/str-filing")

from contextlib import asynccontextmanager

_stop = threading.Event()


@asynccontextmanager
async def lifespan(app_):
    worker.refresh_dlq_depth()
    if os.environ.get("STR_WORKER_ENABLED", "true").lower() == "true":
        start_background(worker)
    bus.start_consumer(intake_event, _stop)
    yield
    _stop.set()


app = FastAPI(title="str-filing", version="1.0.0", lifespan=lifespan)

# fail-closed config validation (prod profile refuses SIM/dev fallbacks)
authz.validate_authz_config()

engine = db.make_engine()
sessions = db.make_session_factory(engine)
adapter = adapter_from_env()
audit = audit_from_env(DATA_DIR)
metrics = Metrics()
permify = authz.permify_from_env()
worker = FilingWorker(sessions, adapter, audit, metrics)


class IdempotencyPayloadConflict(ValueError):
    """w2 #6: same (tenant_id, idempotency_key) replayed with a DIFFERENT
    payload. Subclasses ValueError so the Kafka consumer treats it as a
    poison message (commit past); the REST handler below maps it to 409."""


@app.exception_handler(IdempotencyPayloadConflict)
async def idem_conflict_exc(request: Request, exc: IdempotencyPayloadConflict):
    return authz.problem(409, "conflict", str(exc))


@app.exception_handler(ValueError)
async def value_exc(request: Request, exc: ValueError):
    return authz.problem(422, "unprocessable", str(exc))


@app.get("/healthz")
def healthz():
    return {"status": "ok", "service": "str-filing",
            "profile": "prod" if os.environ.get("AUTH_MODE") == "keycloak"
            else "dev",
            "nfiu_transport": adapter.transport,
            "kafka_intake": bus.kafka_enabled(),
            "permify": "live" if permify else "dev-role-fallback"}


# ---------- intake ----------

class STRIn(BaseModel):
    tenant_id: str = ""
    idempotency_key: str = Field(min_length=1)
    subject_ref: str = ""
    report_type: str = "STR"
    payload: dict = {}
    actor: str = ""


def _canonical_payload(payload: dict) -> str:
    return json.dumps(payload, sort_keys=True, separators=(",", ":"))


def intake_event(event: dict, *, actor: str) -> tuple[dict, bool]:
    """Shared intake for REST + Kafka paths. Idempotent on
    (tenant_id, idempotency_key): duplicates return the existing record
    with created=False and never enqueue a second filing."""
    body = STRIn(**event)
    actor = body.actor or actor
    if not actor:
        raise ValueError("actor is required")
    payload_raw = _canonical_payload(body.payload)
    payload_hash = hashlib.sha256(payload_raw.encode()).hexdigest()
    with sessions() as s:
        existing = (s.query(db.STRFiling)
                    .filter_by(tenant_id=body.tenant_id,
                               idempotency_key=body.idempotency_key)
                    .one_or_none())
        if existing is not None:
            # w2 #6: payload binding — the stored payload_hash is compared,
            # not just the key. Same key + different payload -> 409, never a
            # silent replay of the original filing.
            if existing.payload_hash and existing.payload_hash != payload_hash:
                raise IdempotencyPayloadConflict(
                    "idempotency key already used with a different payload")
            return existing.to_dict(), False
        rec = db.STRFiling(
            tenant_id=body.tenant_id,
            idempotency_key=body.idempotency_key,
            subject_ref=body.subject_ref,
            report_type=body.report_type or "STR",
            payload=payload_raw, payload_hash=payload_hash,
            created_by=actor,
            max_attempts=int(os.environ.get("STR_MAX_ATTEMPTS", "5")),
        )
        s.add(rec)
        s.flush()
        audit.record(actor=actor, str_id=rec.id, tenant_id=rec.tenant_id,
                     old_status="", new_status=db.STATUS_PENDING,
                     str_hash=payload_hash, detail="str created")
        s.commit()
        return rec.to_dict(), True


@app.post("/v1/str", status_code=201)
def create_str(body: STRIn, request: Request):
    rec, created = intake_event(body.model_dump(),
                                actor=request.headers.get("X-Actor", ""))
    return JSONResponse(status_code=201 if created else 200, content=rec)


@app.get("/v1/str/{str_id}")
def get_str(str_id: str):
    with sessions() as s:
        rec = s.get(db.STRFiling, str_id)
        if rec is None:
            return authz.problem(404, "not found", str_id)
        return rec.to_dict()


@app.get("/v1/str")
def list_str(status: str = "", tenant_id: str = ""):
    with sessions() as s:
        q = s.query(db.STRFiling)
        if status:
            q = q.filter(db.STRFiling.status == status)
        if tenant_id:
            q = q.filter(db.STRFiling.tenant_id == tenant_id)
        return [r.to_dict() for r in
                q.order_by(db.STRFiling.created_at).limit(500).all()]


# ---------- DLQ management ----------

@app.post("/v1/str/{str_id}/requeue")
def requeue_str(str_id: str, request: Request):
    """Manual dlq->pending requeue. RBAC-gated: Permify
    str_filing#requeue when PERMIFY_URL is set (case-mgmt checkRel
    pattern), else dev role fallback (admin|compliance-officer)."""
    decision = authz.authorize_requeue(request, str_id, permify)
    if isinstance(decision, JSONResponse):
        return decision
    try:
        rec = worker.requeue(str_id, actor=decision.get("sub", "unknown"))
    except ValueError as exc:
        return authz.problem(409, "conflict", str(exc))
    if rec is None:
        return authz.problem(404, "not found", str_id)
    return rec.to_dict()


@app.get("/v1/str/dlq/depth")
def dlq_depth():
    return {"dlq_depth": worker.refresh_dlq_depth()}


# ---------- SIM-only outage control (runbook / tests) ----------

@app.post("/v1/str/sim/outage")
def sim_outage(body: dict):
    """SIM-only: toggle the simulated NFIU endpoint up/down. Refused when
    the real HTTP adapter is active (tagged SIM per honesty convention)."""
    if not isinstance(adapter, SimNFIUClient):
        return authz.problem(409, "not a SIM deployment",
                             "outage simulation requires STR_NFIU_ADAPTER=sim")
    adapter.available = bool(body.get("available", True))
    adapter.fail_permanent = bool(body.get("fail_permanent", False))
    return {"sim": True, "available": adapter.available,
            "fail_permanent": adapter.fail_permanent}


# ---------- metrics ----------

@app.get("/metrics")
def prom_metrics():
    worker.refresh_dlq_depth()
    return PlainTextResponse(generate_latest(metrics.registry),
                             media_type="text/plain; version=0.0.4")
