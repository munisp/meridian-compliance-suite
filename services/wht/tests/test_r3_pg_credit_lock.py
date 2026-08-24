"""R3 verifier regression (wht #45): the atomic INSERT ... SELECT ... WHERE
balance >= need guard is sufficient on SQLite (single-writer serialization)
but races on Postgres READ COMMITTED — both concurrent statements snapshot
before either commits, both insert, and the credit ledger overdraws. Prod
runs Postgres (db.py DATABASE_URL).

Fix: db.acquire_credit_lock() takes pg_advisory_xact_lock on the credit
account key (wht-credit:{tenant}:{vendor_tin}) around the check+insert when
the engine dialect is postgresql; on SQLite it is a deliberate no-op (the
conditional INSERT remains the guard). These tests pin:
- PG dialect: the advisory lock SQL is issued, and issued BEFORE the
  guarded INSERT (ordering is what makes the guard PG-correct)
- SQLite dialect: no lock SQL, conditional-insert path unchanged
- HTTP-level: concurrent applies against an exactly-covering balance never
  overdraw (runs on SQLite; the PG serialization is advisory-lock-by-
  construction, verified above and documented in db.acquire_credit_lock)
"""
from __future__ import annotations

import threading
from unittest import mock

from fastapi.testclient import TestClient

from app import db
from app.main import app

client = TestClient(app)
H = {"X-Dev-Role": "operator"}


class _FakeBind:
    def __init__(self, name: str):
        self.dialect = mock.Mock()
        self.dialect.name = name


class _FakeSession:
    def __init__(self, dialect: str):
        self._bind = _FakeBind(dialect)
        self.executed: list[str] = []

    def get_bind(self):
        return self._bind

    def execute(self, stmt, params=None):
        self.executed.append(str(stmt))
        return mock.Mock()


def test_pg_dialect_takes_advisory_xact_lock():
    sess = _FakeSession("postgresql")
    db.acquire_credit_lock(sess, "t1", "9999999900099")
    assert len(sess.executed) == 1
    assert "pg_advisory_xact_lock" in sess.executed[0]
    assert "hashtext" in sess.executed[0]  # stable int key from account id


def test_sqlite_dialect_is_noop():
    sess = _FakeSession("sqlite")
    db.acquire_credit_lock(sess, "t1", "9999999900099")
    assert sess.executed == []


def test_apply_credit_locks_before_guarded_insert():
    """The advisory lock must precede the balance-checking INSERT inside
    the same transaction; locking after the check would not close the
    READ COMMITTED snapshot race."""
    vendor = "9999999900100"
    with db.session() as s:
        from app import db as _db
        s.add(_db.Credit(id="cr-r3seed1", vendor_tin=vendor,
                         credit_kobo=1_000_00, source="seed",
                         created_at=db.now()))
        s.commit()
    calls: list[str] = []
    real = db.acquire_credit_lock

    def spy(sess, tenant, vtin):
        # record relative order using the session's connection identity
        calls.append("lock")
        return real(sess, tenant, vtin)

    import app.main as main_mod
    orig_execute = __import__("sqlalchemy").orm.Session.execute

    def exec_spy(self, stmt, *a, **kw):
        if "INSERT INTO wht_credits" in str(stmt):
            calls.append("insert")
        return orig_execute(self, stmt, *a, **kw)

    with mock.patch.object(main_mod.db, "acquire_credit_lock", spy), \
         mock.patch("sqlalchemy.orm.Session.execute", exec_spy):
        r = client.post(f"/v1/wht/credits/{vendor}/apply", headers=H,
                        json={"amount_kobo": 500_00,
                              "idempotency_key": "r3-lock-order-1"})
        assert r.status_code == 201, r.text
    assert calls[:2] == ["lock", "insert"], calls


def test_concurrent_apply_never_overdraws_sqlite():
    """Attack pattern on the test engine: balance covers exactly one
    apply; two concurrent applies must yield exactly one success."""
    vendor = "9999999900101"
    with db.session() as s:
        s.add(db.Credit(id="cr-r3seed2", vendor_tin=vendor,
                        credit_kobo=1_000_00, source="seed",
                        created_at=db.now()))
        s.commit()
    results = []
    barrier = threading.Barrier(2)

    def hit(key: str):
        barrier.wait()
        r = client.post(f"/v1/wht/credits/{vendor}/apply", headers=H,
                        json={"amount_kobo": 1_000_00,
                              "idempotency_key": key})
        results.append(r.status_code)

    threads = [threading.Thread(target=hit, args=(f"r3-race-{i}",))
               for i in range(2)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert sorted(results) == [201, 422], results
    with db.session() as s:
        assert db.credit_balance(s, vendor) == 0
