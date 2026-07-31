"""Tests for the filings service (F1 VAT-002, F2 PAYE/H1, F3 CIT, F4
assessment lifecycle). Integer kobo, round-half-up expectations."""
from datetime import date
from decimal import Decimal

import pytest
from fastapi.testclient import TestClient

from app import assessment, cit, paye, util, vat
from app.main import app

client = TestClient(app)


# ---------- util ----------
def test_kobo_mul_rounds_half_up():
    # 101 * 25% = 25.25 -> 25 ; 103 * 50bps(5%) = 5.15 -> 5; half-up case:
    assert util.kobo_mul(101, 2500) == 25
    assert util.kobo_mul(1050, 5) == 1      # 0.525 -> 1 (half-up)
    assert util.kobo_mul(999, 0) == 0


def test_period_and_deadline():
    assert util.deadline_nth_of_following_month("2026-01", 21) == date(2026, 2, 21)
    assert util.deadline_nth_of_following_month("2026-12", 10) == date(2027, 1, 10)
    with pytest.raises(ValueError):
        util.parse_period("2026-13")


# ---------- F1 VAT ----------
INVOICES = [
    {"irn": "IRN1", "direction": "sale", "basket": "standard_75",
     "net_kobo": 10_000_000_00, "vat_kobo": 750_000_00},
    {"irn": "IRN2", "direction": "sale", "basket": "zero_rated",
     "net_kobo": 5_000_000_00, "vat_kobo": 0},
    {"irn": "IRN3", "direction": "sale", "basket": "exempt",
     "net_kobo": 2_000_000_00, "vat_kobo": 0},
    {"irn": "IRN4", "direction": "purchase", "basket": "standard_75",
     "net_kobo": 4_000_000_00, "vat_kobo": 300_000_00},
    {"irn": "IRN5", "direction": "purchase", "basket": "standard_75",
     "net_kobo": 1_000_000_00, "vat_kobo": 75_000_00,
     "exempt_attributable": True},
]


def test_vat_return_from_einvoice_data():
    ret = vat.build_return("TIN-1", "2026-02", INVOICES)
    assert ret["form"] == "VAT-002"
    assert ret["deadline"] == "2026-03-21"
    s = ret["sales_schedule"]
    assert s["standard_sales_kobo"] == 10_000_000_00
    assert s["zero_rated_sales_kobo"] == 5_000_000_00
    assert s["exempt_sales_kobo"] == 2_000_000_00
    assert ret["output_vat_kobo"] == 750_000_00
    # input: gross 375k, exempt-attributable 75k excluded -> 300k deductible
    assert ret["input_vat"]["gross_input_vat_kobo"] == 375_000_00
    assert ret["input_vat"]["non_deductible_exclusions_kobo"] == 75_000_00
    assert ret["input_vat"]["deductible_input_vat_kobo"] == 300_000_00
    assert ret["net_vat_payable_kobo"] == 450_000_00
    assert ret["refund_kobo"] == 0
    assert not ret["nil_return"]


def test_vat_adjustments_and_refund():
    ret = vat.build_return("TIN-1", "2026-02", INVOICES,
                           adjustments=[{"kind": "credit_note", "vat_kobo": 200_000_00, "ref": "CN-9"},
                                        {"kind": "bad_debt_relief", "vat_kobo": 50_000_00}])
    # 750k output - 300k input - 200k credit + 50k bad-debt = 300k
    assert ret["net_vat_payable_kobo"] == 300_000_00
    refund = vat.build_return("TIN-1", "2026-02", [],
                              purchases=[{"vat_kobo": 900_000_00}],
                              adjustments=[])
    assert refund["refund_kobo"] == 900_000_00
    assert refund["net_vat_payable_kobo"] == 0
    with pytest.raises(vat.VatError):
        vat.build_return("TIN-1", "2026-02", [], adjustments=[{"kind": "bogus", "vat_kobo": 1}])


def test_vat_nil_return_and_schedule_path():
    ret = vat.build_return("TIN-9", "2026-01")
    assert ret["nil_return"]
    sched = vat.build_return("TIN-9", "2026-01", sales_schedule={
        "standard_sales_kobo": 1_000_000_00, "output_vat_kobo": 75_000_00})
    assert sched["output_vat_kobo"] == 75_000_00
    assert sched["source"] == "schedules"


def test_vat_filing_idempotency_and_amendment():
    body = {"tin": "TIN-API", "period": "2026-03", "idempotency_key": "k1",
            "invoices": INVOICES}
    r1 = client.post("/v1/filings/vat", json=body)
    assert r1.status_code == 201
    r2 = client.post("/v1/filings/vat", json=body)
    assert r2.status_code == 200  # idempotent replay
    assert r1.json()["return_id"] == r2.json()["return_id"]
    # new key, same period without amendment flag -> 422
    dup = dict(body, idempotency_key="k2")
    assert client.post("/v1/filings/vat", json=dup).status_code == 422
    # amendment supersedes, same period, version 2
    amd = dict(body, idempotency_key="k3",
               amendment_of=r1.json()["return_id"])
    r3 = client.post("/v1/filings/vat", json=amd)
    assert r3.status_code == 201
    assert r3.json()["version"] == 2
    assert r3.json()["amends"] == r1.json()["return_id"]
    got = client.get("/v1/filings/vat/TIN-API/2026-03")
    assert got.json()["version"] == 2
    assert client.get("/v1/filings/vat/TIN-API/2099-01").status_code == 404


# ---------- F2 PAYE ----------
EMPLOYEES = [
    {"tin": "E1", "name": "Ada", "gross_kobo": 500_000_00, "pension_kobo": 40_000_00},
    {"tin": "E2", "name": "Bello", "gross_kobo": 150_000_00},
]


def test_paye_employee_tax_bands_and_cra():
    r = paye.employee_annual_tax(3_600_000_00, {}, date(2026, 1, 1))
    # CRA = max(200k, 36k) + 720k = 920k; taxable = 2.68m
    assert r["cra_kobo"] == 920_000_00
    assert r["taxable_income_kobo"] == 2_680_000_00
    expected = (300_000_00 * 700 + 300_000_00 * 1100 + 500_000_00 * 1500
                + 500_000_00 * 1900 + 1_080_000_00 * 2100) // 10_000
    assert r["annual_tax_kobo"] == expected
    assert r["monthly_tax_kobo"] * 12 >= expected  # half-up rounding


def test_paye_minimum_tax_when_no_taxable():
    r = paye.employee_annual_tax(150_000_00, {}, date(2026, 1, 1))
    # CRA alone (max(200k,1.5k)+30k=230k) > gross -> taxable 0 -> 1% min tax
    assert r["taxable_income_kobo"] == 0
    assert r["annual_tax_kobo"] == 1_500_00


def test_paye_monthly_schedule_and_deadline():
    sched = paye.build_monthly_schedule("EMP-1", "2026-04", EMPLOYEES)
    assert sched["deadline"] == "2026-05-10"
    assert sched["totals"]["employees"] == 2
    assert sched["totals"]["gross_kobo"] == 650_000_00
    assert sched["totals"]["tax_kobo"] == sum(r["tax_kobo"] for r in sched["rows"])
    row = sched["rows"][0]
    assert {"tin", "name", "gross_kobo", "pension_kobo", "reliefs_kobo",
            "tax_kobo"} <= set(row)


def test_paye_h1_annual_aggregation():
    store = paye.PayeReturnStore()
    for m in (1, 2, 3):
        sched = paye.build_monthly_schedule("EMP-1", f"2026-{m:02d}", EMPLOYEES)
        store.file(sched, f"idem-{m}")
    h1 = paye.build_form_h1("EMP-1", 2026, store.for_year("EMP-1", 2026))
    assert h1["form"] == "H1"
    assert h1["deadline"] == "2027-01-31"
    assert h1["totals"]["gross_kobo"] == 3 * 650_000_00
    ada = next(r for r in h1["rows"] if r["tin"] == "E1")
    assert ada["months"] == 3
    # API path
    r = client.post("/v1/filings/paye", json={
        "employer_tin": "EMP-API", "period": "2026-01",
        "idempotency_key": "p1", "employees": EMPLOYEES})
    assert r.status_code == 201
    assert client.post("/v1/filings/paye", json={
        "employer_tin": "EMP-API", "period": "2026-01",
        "idempotency_key": "p1", "employees": EMPLOYEES}).status_code == 200
    h1r = client.post("/v1/filings/paye/h1", params={"employer_tin": "EMP-API", "year": 2026})
    assert h1r.json()["totals"]["employees"] == 2


# ---------- F3 CIT ----------
def test_cit_capital_allowance_rates_as_data():
    ca = cit.capital_allowance([{"class": "plant_machinery", "cost_kobo": 10_000_000_00}],
                               date(2026, 12, 31))
    assert ca["initial_allowance_kobo"] == 5_000_000_00       # 50%
    assert ca["annual_allowance_kobo"] == 1_250_000_00        # 25% of residue
    with pytest.raises(cit.CitError):
        cit.capital_allowance([{"class": "yacht", "cost_kobo": 1}], date(2026, 1, 1))


def test_cit_loss_relief_effective_dated_4yr_cap():
    losses = [{"year_incurred": 2021, "amount_kobo": 500_000_00},   # pre-NTA loss: 4yr cap
              {"year_incurred": 2025, "amount_kobo": 300_000_00}]
    lr = cit.loss_relief(losses, 2026, 1_000_000_00)
    # 2021 loss is 5 years old in 2026 -> expired under pre-NTA 4-yr rule
    assert lr["expired_kobo"] == 500_000_00
    assert lr["loss_relief_kobo"] == 300_000_00
    # a 2026 loss has no time cap (NTA 2025)
    lr2 = cit.loss_relief([{"year_incurred": 2026, "amount_kobo": 100}], 2035, 100)
    assert lr2["loss_relief_kobo"] == 100


def test_cit_full_chain_standard_company():
    ret = cit.compute_return(
        "C-1", date(2026, 12, 31),
        assessable_profit_kobo=100_000_000_00,
        turnover_kobo=500_000_000_00,
        total_fixed_assets_kobo=100_000_000_00,
        assets=[{"class": "motor_vehicle", "cost_kobo": 20_000_000_00}],
        losses=[{"year_incurred": 2024, "amount_kobo": 5_000_000_00}])
    # CA: IA 50% = 10m; AA 25% of 10m = 2.5m -> 12.5m
    assert ret["capital_allowance_claimed_kobo"] == 12_500_000_00
    # 100m - 12.5m - 5m = 82.5m total profit; CIT 30% = 24.75m
    assert ret["total_profit_kobo"] == 82_500_000_00
    assert ret["company_tier"] == "standard"
    assert ret["cit_kobo"] == 24_750_000_00
    # min tax floor 0.5% of 500m = 2.5m < 24.75m -> not applied
    assert not ret["minimum_tax"]["applied"]
    # dev levy 4% of assessable = 4m
    assert ret["development_levy_kobo"] == 4_000_000_00
    assert ret["effective_tax_payable_kobo"] == 28_750_000_00
    assert ret["deadline"] == "2027-06-30"
    assert "AFTER" in ret["pillar_two_note"]


def test_cit_minimum_tax_floor_applies():
    ret = cit.compute_return("C-2", date(2026, 12, 31),
                             assessable_profit_kobo=100_000_00,
                             turnover_kobo=600_000_000_00,
                             total_fixed_assets_kobo=50_000_000_00)
    # CIT 30% of 100k = 30k; floor = 0.5% of 600m = 3m -> floor wins
    assert ret["minimum_tax"]["applied"]
    assert ret["cit_kobo"] == 3_000_000_00


def test_cit_small_company_zero_rated_and_devlevy():
    ret = cit.compute_return("C-3", date(2026, 12, 31),
                             assessable_profit_kobo=20_000_000_00,
                             turnover_kobo=90_000_000_00,
                             total_fixed_assets_kobo=40_000_000_00)
    assert ret["company_tier"] == "small"
    assert ret["cit_kobo"] == 0
    assert ret["minimum_tax"]["exempt_small_company"]
    assert ret["development_levy_kobo"] == 800_000_00  # 4% of 20m
    # API path + RFC7807 on bad input
    ok = client.post("/v1/filings/cit/compute", json={
        "tin": "C-3", "fye": "2026-12-31",
        "assessable_profit_kobo": 20_000_000_00,
        "turnover_kobo": 90_000_000_00,
        "total_fixed_assets_kobo": 40_000_000_00})
    assert ok.status_code == 200
    bad = client.post("/v1/filings/cit/compute", json={
        "tin": "C-3", "fye": "2026-12-31",
        "assessable_profit_kobo": 1, "turnover_kobo": 1,
        "total_fixed_assets_kobo": 1,
        "assets": [{"class": "yacht", "cost_kobo": 1}]})
    assert bad.status_code == 422
    assert bad.headers["content-type"].startswith("application/problem+json")


# ---------- F4 assessment lifecycle ----------
def _issue(store, amount=10_000_000_00):
    return store.issue("TIN-X", "CIT", "2026", "best_of_judgment",
                       amount, "failure to file", "electronic",
                       date(2026, 3, 1))


def test_assessment_issue_and_final_after_deadline():
    s = assessment.AssessmentStore()
    a = _issue(s)
    assert a["demand_notice"]["objection_deadline"] == "2026-03-31"
    events = s.tick(date(2026, 3, 31))
    assert s.get(a["assessment_id"])["status"] == "open"  # deadline day still open
    events = s.tick(date(2026, 4, 1))
    assert events[0]["event"] == "final_and_conclusive"
    assert s.get(a["assessment_id"])["status"] == "final_and_conclusive"
    # late objection rejected
    with pytest.raises(assessment.AssessmentError):
        s.object(a["assessment_id"], "grounds", 0, 0, date(2026, 4, 2))


def test_objection_validation_and_partial_payment():
    s = assessment.AssessmentStore()
    a = _issue(s)
    with pytest.raises(assessment.AssessmentError):  # no grounds
        s.object(a["assessment_id"], "  ", 0, 0, date(2026, 3, 10))
    with pytest.raises(assessment.AssessmentError):  # admitted > assessed
        s.object(a["assessment_id"], "g", 20_000_000_00, 0, date(2026, 3, 10))
    with pytest.raises(assessment.AssessmentError):  # overpayment of admitted
        s.object(a["assessment_id"], "g", 4_000_000_00, 5_000_000_00, date(2026, 3, 10))
    obj = s.object(a["assessment_id"], "wrong turnover basis", 4_000_000_00,
                   4_000_000_00, date(2026, 3, 10))
    assert obj["disputed_amount_kobo"] == 6_000_000_00
    assert obj["decision_deadline"] == "2026-06-08"
    assert s.get(a["assessment_id"])["status"] == "objected"
    kinds = [h["event"] for h in s.get(a["assessment_id"])["history"]]
    assert "partial_payment_admitted" in kinds


def test_objection_decision_paths():
    s = assessment.AssessmentStore()
    a = _issue(s)
    obj = s.object(a["assessment_id"], "g", 4_000_000_00, 0, date(2026, 3, 10))
    with pytest.raises(assessment.AssessmentError):
        s.decide(obj["objection_id"], "partially_upheld", date(2026, 4, 1))  # needs revised amount
    d = s.decide(obj["objection_id"], "partially_upheld", date(2026, 4, 1),
                 revised_amount_kobo=7_000_000_00)
    assert d["status"] == "partially_upheld"
    assert s.get(a["assessment_id"])["amount_kobo"] == 7_000_000_00
    assert s.get(a["assessment_id"])["status"] == "final_and_conclusive"
    # decision after 90-day deadline not allowed (deemed upheld instead)
    s2 = assessment.AssessmentStore()
    a2 = _issue(s2)
    o2 = s2.object(a2["assessment_id"], "g", 0, 0, date(2026, 3, 10))
    with pytest.raises(assessment.AssessmentError):
        s2.decide(o2["objection_id"], "rejected", date(2026, 6, 9))


def test_deemed_upheld_after_90_days_tat_referral():
    s = assessment.AssessmentStore()
    a = _issue(s)
    obj = s.object(a["assessment_id"], "g", 2_000_000_00, 2_000_000_00,
                   date(2026, 3, 10))
    events = s.tick(date(2026, 6, 9))  # day after 90-day deadline
    ref = next(e for e in events if e.get("referral_id"))
    assert ref["basis"].startswith("objection deemed upheld")
    assert obj["status"] == "deemed_upheld"
    # deemed upheld -> assessment reduced to admitted amount
    assert s.get(a["assessment_id"])["amount_kobo"] == 2_000_000_00
    assert s.tat_referrals()[0]["referral_id"] == ref["referral_id"]


def test_assessment_api_rfc7807_and_service_channels():
    bad = client.post("/v1/assessments", json={
        "tin": "T", "tax_type": "VAT", "period": "2026-01",
        "kind": "best_of_judgment", "amount_kobo": 100,
        "grounds": "g", "served_via": "whatsapp", "served_at": "2026-03-01"})
    assert bad.status_code == 422
    assert bad.headers["content-type"].startswith("application/problem+json")
    ok = client.post("/v1/assessments", json={
        "tin": "T", "tax_type": "VAT", "period": "2026-01",
        "kind": "additional", "amount_kobo": 500_000_00,
        "grounds": "under-declared output VAT", "served_via": "registered_post",
        "served_at": "2026-03-01"})
    assert ok.status_code == 201
    aid = ok.json()["assessment_id"]
    obj = client.post(f"/v1/assessments/{aid}/objections", json={
        "grounds": "figures disputed", "admitted_amount_kobo": 200_000_00,
        "paid_admitted_kobo": 200_000_00, "filed_at": "2026-03-15"})
    assert obj.status_code == 201
    tick = client.post("/v1/assessments/tick", params={"today": "2026-07-01"})
    assert any(e.get("referral_id") for e in tick.json()["events"])
    refs = client.get("/v1/assessments/tat-referrals").json()["referrals"]
    assert refs and refs[0]["assessment_id"] == aid
