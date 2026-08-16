"""R4: STR idempotency TTL — expired key treated as new, purge terminal-only."""
import os
import sys
import tempfile
from datetime import timedelta
from pathlib import Path

_TMP = tempfile.mkdtemp(prefix="str-ttl-test-")
os.environ.setdefault("AUTH_MODE", "dev")
os.environ["DATA_DIR"] = _TMP
os.environ["STR_DATABASE_URL"] = f"sqlite:///{_TMP}/test.db"
os.environ["STR_NFIU_ADAPTER"] = "sim"
os.environ["STR_WORKER_ENABLED"] = "false"

svc = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(svc))
sys.path.insert(0, str(svc.parents[1] / "packages" / "py"))

from app import db  # noqa: E402
from app.main import intake_event, sessions  # noqa: E402

TENANT = "t-ttl"


def _create(key, subject="subj-1"):
    rec, created = intake_event({
        "tenant_id": TENANT, "idempotency_key": key,
        "subject_ref": subject, "payload": {"amount": 1},
        "actor": "kyc-engine"}, actor="kyc-engine")
    return rec, created


def _age(key, days, status=None):
    with sessions() as s:
        rec = (s.query(db.STRFiling)
               .filter_by(tenant_id=TENANT, idempotency_key=key).one())
        rec.created_at = db.utcnow() - timedelta(days=days)
        if status:
            rec.status = status
        s.commit()


def test_replay_within_window():
    rec1, created1 = _create("k-fresh")
    rec2, created2 = _create("k-fresh")
    assert created1 and not created2
    assert rec1["id"] == rec2["id"]


def test_expired_terminal_key_treated_as_new():
    rec1, _ = _create("k-old")
    _age("k-old", days=2 * db.IDEMPOTENCY_TTL_DAYS, status=db.STATUS_FILED)
    rec2, created2 = _create("k-old")
    assert created2, "expired terminal key must start a fresh filing"
    assert rec2["id"] != rec1["id"]


def test_expired_inflight_key_retained():
    rec1, _ = _create("k-inflight")
    _age("k-inflight", days=2 * db.IDEMPOTENCY_TTL_DAYS, status=db.STATUS_SUBMITTING)
    rec2, created2 = _create("k-inflight")
    assert not created2, "in-flight filing must still resolve to the original"
    assert rec2["id"] == rec1["id"]


def test_purge_terminal_only():
    _create("k-p1")  # terminal, expired -> purge
    _age("k-p1", days=2 * db.IDEMPOTENCY_TTL_DAYS, status=db.STATUS_FILED)
    _create("k-p2")  # dlq, expired -> purge
    _age("k-p2", days=2 * db.IDEMPOTENCY_TTL_DAYS, status=db.STATUS_DLQ)
    _create("k-p3")  # in-flight, expired -> keep
    _age("k-p3", days=2 * db.IDEMPOTENCY_TTL_DAYS, status=db.STATUS_PENDING)
    _create("k-p4")  # fresh terminal -> keep
    with sessions() as s:
        (s.query(db.STRFiling)
         .filter_by(tenant_id=TENANT, idempotency_key="k-p4")
         .update({"status": db.STATUS_FILED}))
        s.commit()
        n = db.purge_expired_idempotency(s)
    assert n == 2, n
    with sessions() as s:
        keys = {r.idempotency_key for r in s.query(db.STRFiling)
                .filter_by(tenant_id=TENANT).all()}
    assert "k-p1" not in keys and "k-p2" not in keys
    assert {"k-p3", "k-p4"} <= keys
