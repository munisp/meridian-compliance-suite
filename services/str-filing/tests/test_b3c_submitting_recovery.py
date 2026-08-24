"""B3 #9 regression: STR filings stranded in `submitting`.

Before the fix, a worker crash mid-submit (after the pending->submitting
transition, before the outcome was recorded) stranded the row forever:
process_due only selects pending/failed. The recovery sweep requeues
stranded rows as failed; the retry resubmits under the SAME idempotency
key so NFIU dedupes — filed exactly once.
"""
from __future__ import annotations

import hashlib
import json
from datetime import timedelta

from fastapi.testclient import TestClient

from app import db
from app.main import app, metrics, worker  # noqa: F401  (shared fixtures)

client = TestClient(app)

TENANT = "t-bank-1"

# Merge fallout fix: #39 (B2 #2) made every STR route authenticated; these
# tests predate it. Dev-profile officer headers per test_zz_str_authz.py.
OFFICER = {"X-Dev-Role": "compliance-officer", "X-Tenant-Id": TENANT}


def _create(key: str) -> dict:
    resp = client.post("/v1/str", json={
        "tenant_id": TENANT, "idempotency_key": key,
        "subject_ref": "cust-001", "report_type": "STR",
        "payload": {"amount": 9500000, "currency": "NGN",
                    "trigger": "pep_edd", "case_id": "kyc-" + key},
        "actor": "kyc-engine"}, headers=OFFICER)
    assert resp.status_code == 201, resp.text
    return resp.json()


def _force_submitting(str_id: str, *, age_seconds: float):
    """Simulate crash-mid-submit: row stuck in `submitting`, no outcome."""
    with worker.sessions() as s:
        rec = s.get(db.STRFiling, str_id)
        rec.status = db.STATUS_SUBMITTING
        rec.updated_at = db.utcnow() - timedelta(seconds=age_seconds)
        s.commit()


def _sim():
    from app.main import adapter
    return adapter


def test_b3c_01_stuck_submitting_recovered_and_filed_exactly_once():
    rec = _create("b3c-stuck-1")
    _force_submitting(rec["id"], age_seconds=3600)

    # the sweep resumes the stranded row exactly once
    assert worker.recover_submitting() == 1
    resp = client.get(f"/v1/str/{rec['id']}", headers=OFFICER)
    assert resp.json()["status"] == "failed"
    # a second sweep is a no-op (the row left `submitting`)
    assert worker.recover_submitting() == 0

    # the normal retry path now files it — exactly one NFIU submission
    assert worker.process_due() == 1
    body = client.get(f"/v1/str/{rec['id']}", headers=OFFICER).json()
    assert body["status"] == "filed"
    assert body["nfiu_reference"].startswith("SIM-NFIU-REF-")
    subs = [s for s in _sim().submissions if s["str_id"] == rec["id"]]
    assert len(subs) == 1
    assert subs[0]["idempotency_key"] == "b3c-stuck-1"


def test_b3c_02_recent_submitting_not_swept_grace_window():
    rec = _create("b3c-stuck-2")
    _force_submitting(rec["id"], age_seconds=5)  # in-flight submit
    assert worker.recover_submitting() == 0
    body = client.get(f"/v1/str/{rec['id']}", headers=OFFICER).json()
    assert body["status"] == "submitting"  # untouched within grace
    # explicit zero grace (operator-forced) does recover it
    assert worker.recover_submitting(grace_seconds=0) == 1
    assert worker.process_due() == 1
    assert client.get(f"/v1/str/{rec['id']}", headers=OFFICER).json()["status"] == "filed"


def test_b3c_03_crash_mid_submit_then_outage_retries_no_double_file():
    """Crash after NFIU accepted but before the outcome was recorded:
    the resubmit under the same idempotency key must not double-file.
    SIM has no server-side dedupe, so we assert the KEY is stable and the
    row reaches filed via exactly one successful local outcome."""
    rec = _create("b3c-stuck-3")
    _force_submitting(rec["id"], age_seconds=3600)
    _sim().available = False
    try:
        assert worker.recover_submitting() == 1
        assert worker.process_due() == 1  # retry fails -> failed w/ backoff
        assert client.get(f"/v1/str/{rec['id']}", headers=OFFICER).json()["status"] == "failed"
    finally:
        _sim().available = True
    # STR_RETRY_BASE_SECONDS=0 in tests: retry immediately due
    with worker.sessions() as s:
        r = s.get(db.STRFiling, rec["id"])
        r.next_retry_at = db.utcnow()
        s.commit()
    assert worker.process_due() == 1
    body = client.get(f"/v1/str/{rec['id']}", headers=OFFICER).json()
    assert body["status"] == "filed"
    subs = [s for s in _sim().submissions if s["str_id"] == rec["id"]]
    assert all(s["idempotency_key"] == "b3c-stuck-3" for s in subs)
    assert len(subs) == 1  # only the successful retry reached NFIU
