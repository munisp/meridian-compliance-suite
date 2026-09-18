"""Regression (R4 S1a#10): deductions previously produced NO WHT credit
certificate (WHT Regs 2024 reg. 7 requires one per deduction, retrievable
by the vendor) and over-deductions had no refund path. Now every persisted
deduction issues a signed, vendor-retrievable certificate, and a corrected
re-evaluation routes the excess WHT into the vendor credit ledger."""
from __future__ import annotations

from fastapi.testclient import TestClient

from app import db
from app.main import app

client = TestClient(app)
HDRS = {"X-Dev-Role": "operator"}

DED_BODY = {
    "payment_type": "services", "beneficiary": "company",
    "amount_kobo": 10_000_000_00, "supplier_tin": "7770001112223",
    "vendor_name": "Acme Ltd",
}


def _persist(key: str, **kw):
    body = dict(DED_BODY, idempotency_key=key, record=True, **kw)
    r = client.post("/v1/wht/evaluate", json=body, headers=HDRS)
    assert r.status_code == 200, r.text
    return r.json()["deduction_id"]


def test_certificate_issued_and_vendor_retrievable():
    did = _persist("cert-k1")
    r = client.get("/v1/wht/certificates/7770001112223", headers=HDRS)
    certs = [c for c in r.json()["certificates"] if c["deduction_id"] == did]
    assert len(certs) == 1
    cert = certs[0]
    assert cert["regulation"].startswith("WHT Regs 2024")
    assert cert["wht_kobo"] > 0 and cert["vendor_tin"] == "7770001112223"
    assert cert["signature"]
    # verification endpoint validates the HMAC signature
    got = client.get(
        f"/v1/wht/certificates/7770001112223/{cert['certificate_id']}",
        params={"verify": "true"}, headers=HDRS)
    assert got.json()["signature_valid"] is True
    # cross-vendor retrieval is a 404, not a leak
    other = client.get(
        f"/v1/wht/certificates/9999999999999/{cert['certificate_id']}",
        headers=HDRS)
    assert other.status_code == 404


def test_over_deduction_refund_credits_vendor_ledger():
    # Deducted on the VAT-INCLUSIVE gross (legacy facts); the corrected
    # evaluation deducts on the net-of-VAT base, so WHT was over-deducted.
    did = _persist("ref-k1", amount_kobo=10_750_000_00)
    bal0 = client.get("/v1/wht/credits/7770001112223", headers=HDRS
                      ).json()["balance_kobo"]
    corrected = dict(DED_BODY, amount_kobo=10_750_000_00,
                     includes_vat=True, vat_kobo=750_000_00)
    r = client.post("/v1/wht/refunds", json={
        "deduction_id": did, "corrected": corrected,
        "idempotency_key": "ref-idem-1"}, headers=HDRS)
    assert r.status_code == 201, r.text
    refund = r.json()
    assert refund["over_deducted_kobo"] == (
        refund["deducted_wht_kobo"] - refund["corrected_wht_kobo"]) > 0
    assert refund["status"] == "credited"
    # the excess flowed into the existing credit ledger (applyable)
    bal1 = client.get("/v1/wht/credits/7770001112223", headers=HDRS
                      ).json()["balance_kobo"]
    assert bal1 - bal0 == refund["over_deducted_kobo"]
    # idempotent replay: no second credit
    r2 = client.post("/v1/wht/refunds", json={
        "deduction_id": did, "corrected": corrected,
        "idempotency_key": "ref-idem-1"}, headers=HDRS)
    assert r2.status_code == 201
    bal2 = client.get("/v1/wht/credits/7770001112223", headers=HDRS
                      ).json()["balance_kobo"]
    assert bal2 == bal1


def test_refund_refused_when_not_over_deducted():
    did = _persist("ref-k2")
    r = client.post("/v1/wht/refunds", json={
        "deduction_id": did, "corrected": DED_BODY}, headers=HDRS)
    assert r.status_code == 422  # identical facts -> no over-deduction
