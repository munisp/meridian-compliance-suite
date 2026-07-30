from fastapi.testclient import TestClient

from app.main import app

client = TestClient(app)
H = {"X-Dev-Role": "operator"}


def test_health_ready():
    assert client.get("/healthz").json()["service"] == "wht"
    r = client.get("/readyz")
    assert r.status_code == 200 and r.json()["pack"] == "rp-wht-2024@1.0.0"


def test_base_rates_company_vs_individual():
    # services, company, valid TIN, above carve-out -> 2%
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "services", "beneficiary": "company",
        "amount_kobo": 5_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    body = r.json()
    assert body["rate_bps"] == 200
    assert body["wht_kobo"] == 5_000_000_00 * 200 // 10_000  # 2% of N5m
    assert body["deduction_trigger"] == "payment"
    # same, individual -> 5%
    r2 = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "services", "beneficiary": "individual",
        "amount_kobo": 5_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    assert r2.json()["rate_bps"] == 500
    # dividend 10% (amount above the N2m carve-out)
    r3 = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "dividend", "beneficiary": "company",
        "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    assert r3.json()["rate_bps"] == 1000


def test_earlier_of_payment_or_settlement():
    # settlement earlier -> trigger settlement
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "contract", "beneficiary": "company",
        "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-03-15", "settlement_date": "2026-03-01"})
    body = r.json()
    assert body["deduction_trigger"] == "settlement"
    assert body["deduction_date"] == "2026-03-01"
    # payment earlier -> trigger payment
    r2 = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "contract", "beneficiary": "company",
        "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-03-01", "settlement_date": "2026-03-15"})
    assert r2.json()["deduction_trigger"] == "payment"
    assert r2.json()["deduction_date"] == "2026-03-01"


def test_no_tin_double_rate():
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "commission", "beneficiary": "company",
        "amount_kobo": 5_000_000_00, "payment_date": "2026-02-10"})
    body = r.json()
    assert body["rate_bps"] == 400  # 2% doubled
    assert body["no_tin_double_applied"] is True


def test_nin_acceptable_for_individual():
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "services", "beneficiary": "individual",
        "amount_kobo": 5_000_000_00, "nin": "12345678901",
        "payment_date": "2026-02-10"})
    body = r.json()
    assert body["rate_bps"] == 500  # NOT doubled
    assert body["no_tin_double_applied"] is False
    assert body["identity"]["nin_accepted"] is True


def test_small_company_carveout():
    # <= N2m/month with valid TIN -> exempt via carve-out
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "goods", "beneficiary": "company",
        "amount_kobo": 1_500_000_00, "monthly_amount_kobo": 1_500_000_00,
        "supplier_tin": "1234567890123", "payment_date": "2026-02-10"})
    body = r.json()
    assert body["rate_bps"] == 0
    assert body["small_company_carveout"] is True
    assert body["wht_kobo"] == 0
    # without valid TIN, carve-out does not apply -> base rate doubled
    r2 = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "goods", "beneficiary": "company",
        "amount_kobo": 1_500_000_00, "monthly_amount_kobo": 1_500_000_00,
        "payment_date": "2026-02-10"})
    body2 = r2.json()
    assert body2["small_company_carveout"] is False
    assert body2["rate_bps"] == 400  # 2% x2 (no TIN)


def test_exemptions():
    # direct debit exemption
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "services", "beneficiary": "company",
        "amount_kobo": 50_000_000_00, "supplier_tin": "1234567890123",
        "via_direct_debit": True, "payment_date": "2026-02-10"})
    assert r.json()["exempt"] is True and r.json()["rate_bps"] == 0
    # broker exemption
    r2 = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "commission", "beneficiary": "company",
        "amount_kobo": 50_000_000_00, "supplier_tin": "1234567890123",
        "via_broker": True, "payment_date": "2026-02-10"})
    assert r2.json()["exempt"] is True
    # manufacturer selling own goods
    r3 = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "goods", "beneficiary": "company",
        "amount_kobo": 50_000_000_00, "supplier_tin": "1234567890123",
        "supplier_is_manufacturer": True, "payment_date": "2026-02-10"})
    assert r3.json()["exempt"] is True


def test_tin_verification():
    ok = client.post("/v1/wht/vendors/verify-tin", headers=H,
                     json={"tin": "1234567890123"})
    assert ok.json()["valid"] is True
    bad = client.post("/v1/wht/vendors/verify-tin", headers=H,
                      json={"tin": "123"})
    assert bad.json()["valid"] is False


def test_ledger_credits_and_remit_file():
    # record two deductions
    for i in range(2):
        r = client.post("/v1/wht/deductions", headers=H, json={
            "payment_type": "services", "beneficiary": "company",
            "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
            "vendor_name": "Acme Ltd", "payment_date": "2026-02-1%d" % i})
        assert r.status_code == 201
    # credits before remit: none yet
    bal = client.get("/v1/wht/credits/1234567890123", headers=H).json()
    assert bal["balance_kobo"] == 0
    # generate remittance file via workflow
    rf = client.post("/v1/wht/remit-file", headers=H, json={"period": "2026-02"})
    assert rf.status_code == 201
    body = rf.json()
    per_ded = 10_000_000_00 * 200 // 10_000  # 2% of N10m = 20,000,000 kobo
    assert body["total_wht_kobo"] == 2 * per_ded
    assert "batch_id,vendor_tin" in body["files"]["csv"]
    assert "<WhtRemittance" in body["files"]["xml"]
    # credits posted after remit
    bal2 = client.get("/v1/wht/credits/1234567890123", headers=H).json()
    assert bal2["balance_kobo"] == 2 * per_ded
    # apply credit
    ap = client.post("/v1/wht/credits/1234567890123/apply", headers=H,
                     json={"amount_kobo": per_ded, "note": "offset CIT"})
    assert ap.status_code == 201
    assert ap.json()["balance_kobo"] == per_ded
    # over-apply rejected
    over = client.post("/v1/wht/credits/1234567890123/apply", headers=H,
                       json={"amount_kobo": 999_000_000})
    assert over.status_code == 422
    # workflow history
    wf = client.get("/v1/wht/workflows", headers=H).json()
    assert wf["registered"] == ["wf-wht-remit-schedule"]
    assert len(wf["runs"]) >= 1
