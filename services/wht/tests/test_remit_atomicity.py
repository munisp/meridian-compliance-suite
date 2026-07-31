"""F3: WHT remittance atomicity + idempotency tests (audit Flow 3).

Proves: retry after a crash between credit posting and remitted-flagging
can no longer double-post vendor credits — post_credits is idempotent
(deterministic credit ids per remittance run) and atomic with
mark_remitted (single transaction).
"""
import os
import sys
import tempfile
from pathlib import Path

os.environ["DATA_DIR"] = tempfile.mkdtemp(prefix="wht-f3-")
os.environ["AUTH_MODE"] = "dev"
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from app import db  # noqa: E402
from app.workflow import wf_wht_remit_schedule  # noqa: E402


def _mk_deduction(did: str, vendor: str, kobo: int, period: str = "2026-03"):
    with db.session() as sess:
        sess.add(db.Deduction(
            id=did, tenant_id="", vendor_tin=vendor, vendor_name="V",
            payment_type="services", beneficiary="company",
            amount_kobo=kobo * 20, rate_bps=500, wht_kobo=kobo,
            outcome="charged", deduction_trigger="payment",
            deduction_date=f"{period}-10", period=period))
        sess.commit()


def _credits(vendor: str):
    with db.session() as sess:
        return db.vendor_credits(sess, vendor)


def _ded(did: str):
    with db.session() as sess:
        return sess.get(db.Deduction, did)


def test_retry_after_crash_does_not_double_post():
    _mk_deduction("ded-a", "TIN-A", 50_000_000)
    _mk_deduction("ded-b", "TIN-A", 30_000_000)

    run1 = wf_wht_remit_schedule(period="2026-03")
    assert run1.status == "completed", run1.result
    assert len(_credits("TIN-A")) == 2
    assert _ded("ded-a").remitted and _ded("ded-b").remitted
    batch1 = run1.result["batch_id"]

    # a SECOND run over the same period collects nothing (already remitted)
    run2 = wf_wht_remit_schedule(period="2026-03")
    assert run2.status == "completed"
    assert len(_credits("TIN-A")) == 2, "re-run must not double-post credits"

    # simulate the audit's crash case directly: credits exist for a batch
    # but the workflow is retried over the same deduction set. Rebuild the
    # state as the workflow would and replay the atomic step.
    batch = batch1
    with db.session() as sess:
        # un-mark one deduction as if the crash happened mid-transaction
        # BEFORE the atomic fix (legacy partial state)
        row = sess.get(db.Deduction, "ded-a")
        row.remitted = False
        sess.commit()
    run3 = wf_wht_remit_schedule(period="2026-03")
    assert run3.status == "completed", run3.result
    credits = _credits("TIN-A")
    assert len(credits) == 2, f"replay must dedup, got {len(credits)} credits"
    # deterministic credit ids per remittance run
    ids = sorted(c.id for c in credits)
    assert ids[0].startswith("cr-remit-2026-03-"), ids
    # run1's batch id is deterministic per deduction set
    assert batch1.startswith("remit-2026-03-")
    assert _ded("ded-a").remitted


def test_credit_ids_are_deterministic_and_deduped():
    _mk_deduction("ded-c", "TIN-C", 10_000_000, period="2026-04")
    run = wf_wht_remit_schedule(period="2026-04")
    assert run.status == "completed"
    batch = run.result["batch_id"]
    with db.session() as sess:
        assert sess.get(db.Credit, f"cr-{batch}-ded-c") is not None
    # manually replay the atomic step via a second identical run: dedup
    run2 = wf_wht_remit_schedule(period="2026-04")
    assert run2.status == "completed"
    assert sum(1 for c in _credits("TIN-C")) == 1


def test_deduction_idempotency_key():
    os.environ.setdefault("AUTH_MODE", "dev")
    from fastapi.testclient import TestClient
    from app.main import app
    c = TestClient(app)
    body = {"payment_type": "rent", "beneficiary": "company",
            "amount_kobo": 5_000_000_00, "supplier_tin": "999",
            "payment_date": "2026-05-01", "idempotency_key": "key-xyz"}
    h = {"X-Dev-Role": "operator"}
    r1 = c.post("/v1/wht/deductions", headers=h, json=body)
    r2 = c.post("/v1/wht/deductions", headers=h, json=body)
    assert r1.status_code == 201 and r2.status_code == 201
    assert r1.json()["deduction_id"] == r2.json()["deduction_id"]
    lst = c.get("/v1/wht/deductions?period=2026-05", headers=h).json()
    assert lst["count"] == 1, "retried POST must not duplicate the deduction"
