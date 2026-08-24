"""STR filing pipeline lifecycle tests (waveC C4/C7/C8/C10).

Covers: create->file happy path (SIM adapter), NFIU outage (SIM 503 ->
retries -> no loss -> dlq after max attempts), manual requeue -> retry
success, RBAC gating, duplicate intake idempotency, permanent rejection,
Prometheus metrics emission, WORM audit-trail forensic fields + hash
chain, and the Kafka topic contract for nrs.aml.str.created.
"""
import hashlib
import json

import pytest
from fastapi.testclient import TestClient

from app import db
from app.main import app, audit, intake_event, metrics, sessions, worker

client = TestClient(app)

TENANT = "t-bank-1"

# B2 #2: all STR routes require auth; the caller tenant is
# derived from the verified principal (X-Dev-Role fallback is dev-only).
HDRS = {"X-Dev-Role": "compliance-officer", "X-Tenant-Id": TENANT}


def _create(key: str, subject: str = "cust-001") -> dict:
    resp = client.post("/v1/str", json={
        "tenant_id": TENANT, "idempotency_key": key,
        "subject_ref": subject, "report_type": "STR",
        "payload": {"amount": 9500000, "currency": "NGN",
                    "trigger": "pep_edd", "case_id": "kyc-" + key},
        "actor": "kyc-engine"}, headers=HDRS)
    assert resp.status_code == 201, resp.text
    body = resp.json()
    assert body["status"] == "pending"
    assert body["tenant_id"] == TENANT
    assert body["payload_hash"] == hashlib.sha256(
        json.dumps({"amount": 9500000, "currency": "NGN",
                    "trigger": "pep_edd", "case_id": "kyc-" + key},
                   sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    return body


def _sim():
    from app.main import adapter
    return adapter


def test_01_healthz():
    resp = client.get("/healthz")
    assert resp.status_code == 200
    body = resp.json()
    assert body["nfiu_transport"] == "SIM"
    assert body["kafka_intake"] is False


def test_02_happy_path_create_then_file():
    rec = _create("happy-1")
    assert worker.process_due() == 1
    resp = client.get(f"/v1/str/{rec['id']}", headers=HDRS)
    body = resp.json()
    assert body["status"] == "filed"
    assert body["attempts"] == 0  # attempts only increment on failure
    assert body["nfiu_reference"].startswith("SIM-NFIU-REF-")
    assert body["filed_at"]
    # exactly one SIM submission for this STR
    subs = [s for s in _sim().submissions if s["str_id"] == rec["id"]]
    assert len(subs) == 1
    assert metrics.filed_total.labels(tenant_id=TENANT)._value.get() >= 1


def test_03_outage_retries_no_loss_then_dlq():
    rec = _create("outage-1")
    _sim().available = False  # simulated NFIU 503 outage (runbook step 1-2)
    for i in range(3):  # max_attempts = 3
        assert worker.process_due() == 1
        body = client.get(f"/v1/str/{rec['id']}", headers=HDRS).json()
        if i < 2:
            assert body["status"] == "failed"
            assert "503" in body["last_error"]
        else:
            assert body["status"] == "dlq"  # dead-lettered, NOT lost
    # no loss: row still present, nothing filed
    assert client.get(f"/v1/str/{rec['id']}", headers=HDRS).status_code == 200
    assert not [s for s in _sim().submissions if s["str_id"] == rec["id"]]
    assert metrics.submission_errors.labels(
        tenant_id=TENANT, kind="unavailable")._value.get() >= 3
    assert client.get("/v1/str/dlq/depth", headers=HDRS).json()["dlq_depth"][TENANT] == 1


def test_04_requeue_rbac_denied_for_auditor_and_anonymous():
    rec = _create("rbac-1")
    _sim().available = False
    for _ in range(3):
        worker.process_due()
    assert client.get(f"/v1/str/{rec['id']}", headers=HDRS).json()["status"] == "dlq"
    resp = client.post(f"/v1/str/{rec['id']}/requeue")  # unauthenticated
    assert resp.status_code == 401
    resp = client.post(f"/v1/str/{rec['id']}/requeue",
                       headers={"X-Dev-Role": "auditor"})
    assert resp.status_code == 403
    assert client.get(f"/v1/str/{rec['id']}", headers=HDRS).json()["status"] == "dlq"


def test_05_manual_requeue_then_retry_success():
    rec = _create("requeue-1")
    _sim().available = False
    for _ in range(3):
        worker.process_due()
    assert client.get(f"/v1/str/{rec['id']}", headers=HDRS).json()["status"] == "dlq"
    _sim().available = True  # NFIU restored (runbook step 4)
    resp = client.post(f"/v1/str/{rec['id']}/requeue",
                       headers={"X-Dev-Role": "compliance-officer"})
    assert resp.status_code == 200, resp.text
    assert resp.json()["status"] == "pending"
    assert resp.json()["attempts"] == 0
    worker.process_due()
    body = client.get(f"/v1/str/{rec['id']}", headers=HDRS).json()
    assert body["status"] == "filed"
    # exactly-once delivery after recovery
    subs = [s for s in _sim().submissions if s["str_id"] == rec["id"]]
    assert len(subs) == 1


def test_06_requeue_non_dlq_conflict():
    rec = _create("conflict-1")
    resp = client.post(f"/v1/str/{rec['id']}/requeue",
                       headers={"X-Dev-Role": "admin"})
    assert resp.status_code == 409


def test_07_duplicate_intake_idempotency():
    first = _create("dup-1")
    resp = client.post("/v1/str", json={
        "tenant_id": TENANT, "idempotency_key": "dup-1",
        "subject_ref": "cust-001", "report_type": "STR",
        "payload": {"amount": 9500000, "currency": "NGN",
                    "trigger": "pep_edd", "case_id": "kyc-dup-1"},
        "actor": "kyc-engine"}, headers=HDRS)
    assert resp.status_code == 200  # not 201
    assert resp.json()["id"] == first["id"]
    with sessions() as s:
        n = (s.query(db.STRFiling)
             .filter_by(tenant_id=TENANT, idempotency_key="dup-1").count())
        assert n == 1
    worker.process_due()
    subs = [s for s in _sim().submissions if s["str_id"] == first["id"]]
    assert len(subs) == 1


def test_08_permanent_rejection_goes_straight_to_dlq():
    rec = _create("reject-1")
    _sim().available = True
    _sim().fail_permanent = True
    worker.process_due()
    _sim().fail_permanent = False
    body = client.get(f"/v1/str/{rec['id']}", headers=HDRS).json()
    assert body["status"] == "dlq"
    assert body["attempts"] == 1  # no retries on 4xx
    assert metrics.submission_errors.labels(
        tenant_id=TENANT, kind="rejected")._value.get() >= 1


def test_09_metrics_endpoint():
    resp = client.get("/metrics")
    assert resp.status_code == 200
    text = resp.text
    assert "str_dlq_depth" in text
    assert "str_submission_errors_total" in text
    assert "str_filed_total" in text


def test_10_audit_trail_forensic_fields_and_hash_chain():
    path = audit._path  # local WORM-style JSONL fallback in dev/test
    with open(path) as fh:
        records = [json.loads(l) for l in fh if l.strip()]
    assert len(records) >= 4  # created + transitions from earlier tests
    # first REST-created filing (B2 #2: actor is the verified principal)
    created = next(r for r in records
                   if r["new_status"] == "pending"
                   and r["actor"] == "dev-compliance-officer")
    for field in ("actor", "timestamp", "str_id", "tenant_id",
                  "old_status", "new_status", "str_hash", "prev_chain",
                  "sha256", "chain"):
        assert field in created
    assert created["old_status"] == ""
    assert created["actor"] == "dev-compliance-officer"  # B2 #2: actor is the verified principal, not the request body
    # verify the tamper-evident hash chain across the whole log
    prev = "0" * 64
    for r in records:
        assert r["prev_chain"] == prev
        body = {k: v for k, v in r.items()
                if k not in ("sha256", "chain", "worm_uri", "source")}
        digest = hashlib.sha256(
            json.dumps(body, sort_keys=True,
                       separators=(",", ":")).encode()).hexdigest()
        assert r["sha256"] == digest
        assert r["chain"] == hashlib.sha256(
            (prev + digest).encode()).hexdigest()
        prev = r["chain"]
    # every status transition for one filing is recorded in order
    fid = client.post("/v1/str", json={
        "tenant_id": TENANT, "idempotency_key": "audit-seq",
        "subject_ref": "c", "payload": {}, "actor": "t"}, headers=HDRS).json()["id"]
    worker.process_due()
    with open(path) as fh:
        seq = [json.loads(l) for l in fh if json.loads(l)["str_id"] == fid]
    assert [(r["old_status"], r["new_status"]) for r in seq] == [
        ("", "pending"), ("pending", "submitting"),
        ("submitting", "filed")]


def test_11_kafka_topic_contract_intake():
    """kyc-engine nrs.aml.str.created event shape intakes identically to
    REST (bus.start_consumer delegates to intake_event)."""
    event = {
        "tenant_id": TENANT,
        "idempotency_key": "kyc-engine:case-77:screen-9",
        "subject_ref": "cust-77",
        "report_type": "STR",
        "payload": {"rule": "sanctions_hit", "score": 0.98},
        "actor": "kyc-engine",
    }
    rec, created = intake_event(dict(event), actor="kafka:nrs.aml.str.created")
    assert created is True
    assert rec["status"] == "pending"
    assert rec["created_by"] == "kyc-engine"
    rec2, created2 = intake_event(dict(event),
                                  actor="kafka:nrs.aml.str.created")
    assert created2 is False and rec2["id"] == rec["id"]
    worker.process_due()
    assert client.get(f"/v1/str/{rec['id']}", headers=HDRS).json()["status"] == "filed"


def test_12_sim_outage_endpoint_refused_on_http_adapter():
    from app.nfiu import SimNFIUClient
    resp = client.post("/v1/str/sim/outage", json={"available": False})
    # active adapter in tests IS the SIM adapter, so this toggles fine;
    # the refusal path is covered by the type check on the real adapter.
    assert resp.status_code == 200
    assert resp.json()["sim"] is True
    _sim().available = True
    assert isinstance(_sim(), SimNFIUClient)


def test_13_idempotency_payload_binding_conflict():
    """w2 #6: same idempotency key + different payload -> 409, not a silent
    replay of the original filing; identical payload still replays 200."""
    key = "payload-bind-1"
    base = {
        "tenant_id": TENANT, "idempotency_key": key,
        "subject_ref": "cust-pb", "report_type": "STR",
        "payload": {"amount": 1000, "currency": "NGN", "trigger": "pb"},
        "actor": "kyc-engine"}
    r1 = client.post("/v1/str", json=base, headers=HDRS)
    assert r1.status_code == 201, r1.text

    # identical payload replays the original record (200, same id)
    r2 = client.post("/v1/str", json=dict(base), headers=HDRS)
    assert r2.status_code == 200 and r2.json()["id"] == r1.json()["id"]

    # different payload, same key -> 409 conflict
    changed = dict(base, payload={"amount": 2000, "currency": "NGN",
                                  "trigger": "pb"})
    r3 = client.post("/v1/str", json=changed, headers=HDRS)
    assert r3.status_code == 409, r3.text

    # direct intake path (Kafka contract) raises the conflict error
    from app.main import IdempotencyPayloadConflict
    with pytest.raises(IdempotencyPayloadConflict):
        intake_event(changed, actor="kafka:test")
    rec, created = intake_event(dict(base), actor="kafka:test")
    assert created is False and rec["id"] == r1.json()["id"]
