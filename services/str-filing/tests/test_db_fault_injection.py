"""§6.3 db-fault injection + unique-conflict race path for the STR flow
(assurance R7).

Cells closed: db timeout on state write, deadlock on state write, and the
UniqueConstraint(tenant_id, idempotency_key) race path (a concurrent intake
committing the same key between read and write resolves as a payload-bound
replay, never a 500 and never a duplicate).
"""
import os
import sys
import tempfile
from pathlib import Path

_TMP = tempfile.mkdtemp(prefix="str-dbfault-test-")
os.environ.setdefault("AUTH_MODE", "dev")
os.environ["DATA_DIR"] = _TMP
os.environ["STR_DATABASE_URL"] = f"sqlite:///{_TMP}/test.db"
os.environ["STR_NFIU_ADAPTER"] = "sim"
os.environ["STR_WORKER_ENABLED"] = "false"

svc = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(svc))
sys.path.insert(0, str(svc.parents[1] / "packages" / "py"))

import pytest  # noqa: E402
from sqlalchemy.exc import OperationalError  # noqa: E402

from app import db  # noqa: E402
from app.main import IdempotencyPayloadConflict, intake_event, sessions  # noqa: E402

TENANT = "t-dbf"


@pytest.fixture(autouse=True)
def _cleanup():
    yield
    # leave no pending rows behind: the suite shares one dev database and
    # other modules assert exact worker process_due() counts
    with sessions() as s:
        for r in s.query(db.STRFiling).filter_by(tenant_id=TENANT).all():
            s.delete(r)
        s.commit()


def _evt(key, amount=100):
    return {"tenant_id": TENANT, "idempotency_key": key,
            "subject_ref": "subj-1", "payload": {"amount": amount},
            "actor": "kyc-engine"}


def _count(key):
    with sessions() as s:
        return s.query(db.STRFiling).filter_by(
            tenant_id=TENANT, idempotency_key=key).count()


class _FlushFailSession:
    """Session proxy whose flush() raises (db timeout/deadlock simulated at
    the SQLAlchemy boundary); everything else delegates to the real session."""

    def __init__(self, real, exc):
        self._real = real
        self._exc = exc

    def flush(self):
        raise self._exc

    def __getattr__(self, name):
        return getattr(self._real, name)

    def __enter__(self):
        return self

    def __exit__(self, *a):
        self._real.rollback()
        self._real.close()
        return False


def _patch_sessions(monkeypatch, factory):
    import app.main as m
    monkeypatch.setattr(m, "sessions", factory)


def test_db_timeout_on_state_write_surfaces_and_recovers(monkeypatch):
    import app.main as m
    real = m.sessions
    _patch_sessions(monkeypatch, lambda: _FlushFailSession(
        real(), OperationalError("timeout", {}, None)))
    with pytest.raises(OperationalError):
        intake_event(_evt("k-dbt"), actor="kyc-engine")
    monkeypatch.undo()  # db recovers
    # nothing committed under the fault
    assert _count("k-dbt") == 0
    # db recovers: the same event now files cleanly (safe retry)
    rec, created = intake_event(_evt("k-dbt"), actor="kyc-engine")
    assert created and rec["status"] == db.STATUS_PENDING


def test_db_deadlock_on_state_write_retryable(monkeypatch):
    import app.main as m
    real = m.sessions
    _patch_sessions(monkeypatch, lambda: _FlushFailSession(
        real(), OperationalError("deadlock detected", {}, None)))
    with pytest.raises(OperationalError):
        intake_event(_evt("k-dl"), actor="kyc-engine")
    monkeypatch.undo()  # db recovers
    assert _count("k-dl") == 0
    rec, created = intake_event(_evt("k-dl"), actor="kyc-engine")
    assert created
    rec2, created2 = intake_event(_evt("k-dl"), actor="kyc-engine")
    assert not created2 and rec2["id"] == rec["id"]
    assert _count("k-dl") == 1


class _RaceSession:
    """Session proxy simulating the intake race: the idempotency pre-read
    reports ABSENT (the concurrent winner committed after our read), so the
    real flush() hits the (tenant_id, idempotency_key) UniqueConstraint.
    The post-rollback re-query sees the real (winning) row."""

    def __init__(self, real):
        self._real = real
        self._hidden = False

    def query(self, *a, **k):
        q = self._real.query(*a, **k)
        outer = self

        class Q:
            def filter_by(self, *fa, **fk):
                self._q = q.filter_by(*fa, **fk)
                return self

            def one_or_none(self):
                if not outer._hidden:
                    outer._hidden = True  # hide the winner from the pre-read
                    return None
                return self._q.one_or_none()

            def __getattr__(self, name):
                return getattr(self._q, name)

        return Q()

    def __getattr__(self, name):
        return getattr(self._real, name)

    def __enter__(self):
        return self

    def __exit__(self, *a):
        self._real.rollback()
        self._real.close()
        return False


def _race_factory():
    import app.main as m
    real = m.sessions
    return lambda: _RaceSession(real())


def test_unique_conflict_race_resolves_as_replay(monkeypatch):
    winner, created = intake_event(_evt("k-race"), actor="kyc-engine")
    assert created
    _patch_sessions(monkeypatch, _race_factory())
    rec, created = intake_event(_evt("k-race"), actor="kyc-engine")
    assert not created
    assert rec["id"] == winner["id"]  # resolved to the winner, no duplicate
    assert _count("k-race") == 1


def test_unique_conflict_race_different_payload_conflicts(monkeypatch):
    winner, created = intake_event(_evt("k-race2", amount=100), actor="kyc-engine")
    assert created
    _patch_sessions(monkeypatch, _race_factory())
    with pytest.raises(IdempotencyPayloadConflict):
        intake_event(_evt("k-race2", amount=999), actor="kyc-engine")
    assert _count("k-race2") == 1 and winner["id"]
