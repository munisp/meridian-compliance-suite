"""WHT engine tests — canonical rp-wht-2024 pack (byte-identical to rule-packs repo).

Rates per Deduction of Tax at Source (Withholding) Regulations 2024, First Schedule:
  dividend/interest/rent 10%/10%, royalty 10% corp / 5% ind (5% ind also non-resident),
  goods 2%, construction 2% roads/bridges/buildings/power plants vs 5% other
  (non-resident 5%/10%), professional/consultancy/technical/management/commission 5%,
  generic "other services" 2% resident / 5% non-resident, directors' fees 15%/20%,
  winnings 5% resident / 15% non-resident (from 2024-10-01).
"""
from fastapi.testclient import TestClient

from app.main import app

client = TestClient(app)
H = {"X-Dev-Role": "operator"}


def test_health_ready():
    assert client.get("/healthz").json()["service"] == "wht"
    r = client.get("/readyz")
    assert r.status_code == 200 and r.json()["pack"] == "rp-wht-2024@1.0.0"


def test_base_rates_company_vs_individual():
    # generic "other services", company, valid TIN -> 2% (First Schedule row
    # "services other than those specifically listed"; professional/consultancy
    # stay 5% — the 2026-07-31 audit split)
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "services", "beneficiary": "company",
        "amount_kobo": 5_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    body = r.json()
    assert body["rate_bps"] == 200
    assert body["wht_kobo"] == 5_000_000_00 * 200 // 10_000  # 2% of N5m
    assert body["deduction_trigger"] == "payment"
    # services, individual -> 2%
    r2 = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "services", "beneficiary": "individual",
        "amount_kobo": 5_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    assert r2.json()["rate_bps"] == 200
    # professional services stay 5%
    rp = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "professional", "beneficiary": "company",
        "amount_kobo": 5_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    assert rp.json()["rate_bps"] == 500
    # dividend 10%
    r3 = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "dividend", "beneficiary": "company",
        "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    assert r3.json()["rate_bps"] == 1000
    # royalty: company 10%, individual 5% (drift had them swapped)
    r4 = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "royalty", "beneficiary": "company",
        "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    assert r4.json()["rate_bps"] == 1000
    r5 = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "royalty", "beneficiary": "individual",
        "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    assert r5.json()["rate_bps"] == 500
    # goods 2% for BOTH beneficiary classes (drift had 5% ind)
    for bene in ("company", "individual"):
        rr = client.post("/v1/wht/evaluate", headers=H, json={
            "payment_type": "supply_of_goods_materials", "beneficiary": bene,
            "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
            "payment_date": "2026-02-10"})
        assert rr.json()["rate_bps"] == 200, bene
    # construction: roads/bridges/buildings/power plants 2%; any OTHER
    # construction 5% (default when no construction_type supplied)
    rr = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "construction", "beneficiary": "company",
        "construction_type": "roads",
        "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    assert rr.json()["rate_bps"] == 200
    for bene in ("company", "individual"):
        rr = client.post("/v1/wht/evaluate", headers=H, json={
            "payment_type": "construction", "beneficiary": bene,
            "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
            "payment_date": "2026-02-10"})
        assert rr.json()["rate_bps"] == 500, bene


def test_first_schedule_2024_corrections():
    """2026-07-31 audit corrections, behaviorally through the engine."""
    # directors' fees: 15% resident / 20% non-resident individual
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "directors_fees", "beneficiary": "individual",
        "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    assert r.json()["rate_bps"] == 1500
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "directors_fees", "beneficiary": "individual",
        "beneficiary_residence": "non_resident",
        "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    assert r.json()["rate_bps"] == 2000
    # winnings: 5% resident / 15% non-resident (NOT exempt, from 2024-10-01)
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "winnings", "source": "lottery", "beneficiary": "individual",
        "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    body = r.json()
    assert body["rate_bps"] == 500 and body["exempt"] is False
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "winnings", "source": "gaming", "beneficiary": "individual",
        "beneficiary_residence": "non_resident",
        "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    assert r.json()["rate_bps"] == 1500
    # royalty non-resident INDIVIDUAL stays 5% (not clobbered by the 10% NR rule)
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "royalty", "beneficiary": "individual",
        "beneficiary_residence": "non_resident",
        "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    assert r.json()["rate_bps"] == 500
    # construction non-resident: 5% core / 10% other
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "construction", "construction_type": "buildings",
        "beneficiary": "company", "beneficiary_residence": "non_resident",
        "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    assert r.json()["rate_bps"] == 500
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "construction", "construction_type": "other",
        "beneficiary": "company", "beneficiary_residence": "non_resident",
        "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    assert r.json()["rate_bps"] == 1000


def test_legacy_aliases_mapped():
    # goods -> supply_of_goods_materials (2%); contract -> construction
    # (5% — defaults to "any other construction" without a construction_type)
    expected = {"goods": 200, "contract": 500}
    for alias, want in expected.items():
        r = client.post("/v1/wht/evaluate", headers=H, json={
            "payment_type": alias, "beneficiary": "company",
            "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
            "payment_date": "2026-02-10"})
        assert r.json()["rate_bps"] == want, alias


def test_unknown_payment_type_422():
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "service_fee_typo", "beneficiary": "company",
        "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123"})
    assert r.status_code == 422


def test_earlier_of_payment_or_settlement():
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "construction", "beneficiary": "company",
        "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-03-15", "settlement_date": "2026-03-01"})
    body = r.json()
    assert body["deduction_trigger"] == "settlement"
    assert body["deduction_date"] == "2026-03-01"
    r2 = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "construction", "beneficiary": "company",
        "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-03-01", "settlement_date": "2026-03-15"})
    assert r2.json()["deduction_trigger"] == "payment"
    assert r2.json()["deduction_date"] == "2026-03-01"


def test_no_tin_double_rate():
    # commission company 5% -> doubled to 10% without TIN (active income)
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "commission", "beneficiary": "company",
        "amount_kobo": 5_000_000_00, "payment_date": "2026-02-10"})
    body = r.json()
    assert body["rate_bps"] == 1000
    assert body["no_tin_double_applied"] is True


def test_no_tin_double_rate_not_applied_to_passive_income():
    # Regs/PwC: the no-TIN double rate does NOT apply to passive income
    for pt in ("dividend", "interest", "royalty", "rent"):
        r = client.post("/v1/wht/evaluate", headers=H, json={
            "payment_type": pt, "beneficiary": "company",
            "amount_kobo": 5_000_000_00, "payment_date": "2026-02-10"})
        body = r.json()
        assert body["rate_bps"] == 1000, pt
        assert body["no_tin_double_applied"] is False, pt


def test_nin_acceptable_for_individual():
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "services", "beneficiary": "individual",
        "amount_kobo": 5_000_000_00, "nin": "12345678901",
        "payment_date": "2026-02-10"})
    body = r.json()
    assert body["rate_bps"] == 200  # NOT doubled (2% other-services)
    assert body["no_tin_double_applied"] is False
    assert body["identity"]["nin_accepted"] is True


def test_small_company_carveout():
    # PAYER is a small company (<= N25m p.a.), transaction <= N2m in the
    # calendar month, supplier has valid TIN -> no deduction (WHT Regs 2024 reg. 4)
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "supply_of_goods_materials", "beneficiary": "company",
        "amount_kobo": 1_500_000_00,
        "payer_size": "small",
        "payer_annual_turnover_kobo": 20_000_000_00,
        "supplier_tin": "1234567890123", "payment_date": "2026-02-10"})
    body = r.json()
    assert body["rate_bps"] == 0
    assert body["small_company_carveout"] is True
    assert body["wht_kobo"] == 0
    # boundary: transaction value EXACTLY N2m in the month is still carved out (lte)
    rb = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "supply_of_goods_materials", "beneficiary": "company",
        "amount_kobo": 2_000_000_00,
        "payer_size": "small",
        "payer_annual_turnover_kobo": 25_000_000_00,
        "supplier_tin": "1234567890123", "payment_date": "2026-02-10"})
    assert rb.json()["small_company_carveout"] is True
    # REGRESSION: carve-out must NEVER be granted without a supplier TIN
    rn = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "supply_of_goods_materials", "beneficiary": "company",
        "amount_kobo": 1_500_000_00,
        "payer_size": "small",
        "payer_annual_turnover_kobo": 20_000_000_00,
        "payment_date": "2026-02-10"})
    assert rn.json()["small_company_carveout"] is False
    # payer not small -> no carve-out even with TIN and small amount
    r3 = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "supply_of_goods_materials", "beneficiary": "company",
        "amount_kobo": 1_500_000_00,
        "payer_size": "large",
        "payer_annual_turnover_kobo": 500_000_000_00,
        "supplier_tin": "1234567890123", "payment_date": "2026-02-10"})
    assert r3.json()["small_company_carveout"] is False
    assert r3.json()["rate_bps"] == 200
    # audit fix: a <= N2m PAYMENT alone (no payer facts) must NOT carve out
    r2 = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "supply_of_goods_materials", "beneficiary": "company",
        "amount_kobo": 1_500_000_00,
        "supplier_tin": "1234567890123", "payment_date": "2026-02-10"})
    body2 = r2.json()
    assert body2["small_company_carveout"] is False
    assert body2["rate_bps"] == 200
    # legacy supplier-side modeling must NOT trigger the carve-out anymore
    r4 = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "supply_of_goods_materials", "beneficiary": "company",
        "amount_kobo": 1_500_000_00,
        "supplier_size": "small",
        "supplier_monthly_turnover_kobo": 1_500_000_00,
        "supplier_tin": "1234567890123", "payment_date": "2026-02-10"})
    assert r4.json()["small_company_carveout"] is False
    assert r4.json()["rate_bps"] == 200


def test_exemptions():
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "services", "beneficiary": "company",
        "amount_kobo": 50_000_000_00, "supplier_tin": "1234567890123",
        "via_direct_debit": True, "payment_date": "2026-02-10"})
    assert r.json()["exempt"] is True and r.json()["rate_bps"] == 0
    r2 = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "commission", "beneficiary": "company",
        "amount_kobo": 50_000_000_00, "supplier_tin": "1234567890123",
        "via_broker": True, "payment_date": "2026-02-10"})
    assert r2.json()["exempt"] is True
    r3 = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "supply_of_goods_materials", "beneficiary": "company",
        "amount_kobo": 50_000_000_00, "supplier_tin": "1234567890123",
        "supplier_is_manufacturer": True, "payment_date": "2026-02-10"})
    assert r3.json()["exempt"] is True


def test_rounding_half_up_kobo():
    # 2% of 25 kobo = 0.5 kobo -> pack-mandated round() gives 1 (floor gave 0)
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "construction", "beneficiary": "company",
        "amount_kobo": 25, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    assert r.json()["wht_kobo"] == 1


def test_tin_verification():
    ok = client.post("/v1/wht/vendors/verify-tin", headers=H,
                     json={"tin": "1234567890123"})
    assert ok.json()["valid"] is True
    bad = client.post("/v1/wht/vendors/verify-tin", headers=H,
                      json={"tin": "123"})
    assert bad.json()["valid"] is False


def test_ledger_credits_and_remit_file():
    for i in range(2):
        r = client.post("/v1/wht/deductions", headers=H, json={
            "payment_type": "services", "beneficiary": "company",
            "amount_kobo": 10_000_000_00, "supplier_tin": "1234567890123",
            "vendor_name": "Acme Ltd", "payment_date": "2026-02-1%d" % i})
        assert r.status_code == 201
    bal = client.get("/v1/wht/credits/1234567890123", headers=H).json()
    assert bal["balance_kobo"] == 0
    rf = client.post("/v1/wht/remit-file", headers=H, json={"period": "2026-02"})
    assert rf.status_code == 201
    body = rf.json()
    per_ded = 10_000_000_00 * 200 // 10_000  # 2% of N10m = 20,000,000 kobo
    assert body["total_wht_kobo"] == 2 * per_ded
    assert "batch_id,vendor_tin" in body["files"]["csv"]
    assert "<WhtRemittance" in body["files"]["xml"]
    bal2 = client.get("/v1/wht/credits/1234567890123", headers=H).json()
    assert bal2["balance_kobo"] == 2 * per_ded
    ap = client.post("/v1/wht/credits/1234567890123/apply", headers=H,
                     json={"amount_kobo": per_ded, "note": "offset CIT"})
    assert ap.status_code == 201
    assert ap.json()["balance_kobo"] == per_ded
    over = client.post("/v1/wht/credits/1234567890123/apply", headers=H,
                       json={"amount_kobo": 999_000_000})
    assert over.status_code == 422
    wf = client.get("/v1/wht/workflows", headers=H).json()
    assert wf["registered"] == ["wf-wht-remit-schedule"]
    assert len(wf["runs"]) >= 1
