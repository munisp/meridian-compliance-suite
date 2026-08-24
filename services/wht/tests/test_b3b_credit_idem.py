"""B3 #10/#20 regressions: atomic credit-apply guard + apply idempotency;
payload-bound deduction idempotency with no concurrent 500s."""
from __future__ import annotations

import threading

from fastapi.testclient import TestClient

from app.main import app

client = TestClient(app)
H = {"X-Dev-Role": "operator"}

VENDOR = "9999999900001"
DED = {"payment_type": "services", "beneficiary": "company",
       "amount_kobo": 10_000_000_00, "supplier_tin": VENDOR,
       "vendor_name": "Race Ltd", "payment_date": "2026-03-05",
       "idempotency_key": "b3b-ded-1"}


def _fund(vendor: str, kobo: int) -> None:
    """Insert a positive credit row directly (remit-credit equivalent)."""
    import uuid
    from app import db
    with db.session() as sess:
        sess.add(db.Credit(id=f"cr-{uuid.uuid4().hex[:12]}",
                           vendor_tin=vendor, credit_kobo=kobo,
                           source="test", created_at=db.now()))
        sess.commit()


def test_deduction_idempotency_replay_and_conflict():
    r1 = client.post("/v1/wht/deductions", headers=H, json=DED)
    assert r1.status_code == 201
    did = r1.json()["deduction_id"]
    # same payload replay -> same deduction, no double count
    r2 = client.post("/v1/wht/deductions", headers=H, json=DED)
    assert r2.status_code == 201
    assert r2.json()["deduction_id"] == did
    # key reuse with a different payload -> 409, not silent replay
    r3 = client.post("/v1/wht/deductions", headers=H,
                     json=dict(DED, amount_kobo=20_000_000_00))
    assert r3.status_code == 409
    assert r3.headers["content-type"].startswith("application/problem+json")


def test_deduction_concurrent_same_key_no_500():
    """B3 #20: concurrent same-key inserts must replay (201), never 500."""
    body = dict(DED, idempotency_key="b3b-ded-race", supplier_tin="9999999900002",
                payment_date="2026-03-06")
    codes, ids = [], []

    def hit():
        r = client.post("/v1/wht/deductions", headers=H, json=body)
        codes.append(r.status_code)
        if r.status_code == 201:
            ids.append(r.json()["deduction_id"])

    threads = [threading.Thread(target=hit) for _ in range(6)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert all(c == 201 for c in codes), codes
    assert len(set(ids)) == 1


def test_apply_credit_idempotent_and_payload_bound():
    _fund(VENDOR, 500_000)
    body = {"amount_kobo": 200_000, "note": "x", "idempotency_key": "b3b-ap-1"}
    r1 = client.post(f"/v1/wht/credits/{VENDOR}/apply", headers=H, json=body)
    assert r1.status_code == 201
    r2 = client.post(f"/v1/wht/credits/{VENDOR}/apply", headers=H, json=body)
    assert r2.status_code == 201
    assert r2.json()["credit_id"] == r1.json()["credit_id"]
    assert r2.json().get("replayed") is True
    # balance reflects exactly one application
    bal = client.get(f"/v1/wht/credits/{VENDOR}", headers=H).json()
    assert bal["balance_kobo"] == 300_000
    # same key, different amount -> 409
    r3 = client.post(f"/v1/wht/credits/{VENDOR}/apply", headers=H,
                     json=dict(body, amount_kobo=100_000))
    assert r3.status_code == 409


def test_apply_credit_concurrent_never_overdraws():
    """B3 #10: N concurrent applies against a balance that covers exactly
    one must yield exactly one success; the rest 422 (no overdraw)."""
    vendor = "9999999900003"
    _fund(vendor, 100_000)
    codes = []

    def hit(i):
        r = client.post(f"/v1/wht/credits/{vendor}/apply", headers=H,
                        json={"amount_kobo": 100_000,
                              "idempotency_key": f"b3b-race-{i}"})
        codes.append(r.status_code)

    threads = [threading.Thread(target=hit, args=(i,)) for i in range(6)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert codes.count(201) == 1, codes
    assert codes.count(422) == 5, codes
    bal = client.get(f"/v1/wht/credits/{vendor}", headers=H).json()
    assert bal["balance_kobo"] == 0
