from datetime import date

from fastapi.testclient import TestClient

from app.main import app

client = TestClient(app)

INVS = [
    {"invoice_id": "i1", "supplier_tin": "A", "customer_tin": "B", "vat_kobo": 75000},
    {"invoice_id": "i2", "supplier_tin": "B", "customer_tin": "C", "vat_kobo": 50000},
    {"invoice_id": "i3", "supplier_tin": "C", "customer_tin": "A", "vat_kobo": 90000},
    {"invoice_id": "i4", "supplier_tin": "A", "customer_tin": "D", "vat_kobo": 10000},
]


def test_i8_circularity_detects_loop_once():
    r = client.post("/v1/insights/circularity", json={"invoices": INVS})
    body = r.json()
    assert len(body["cycles"]) == 1
    cyc = body["cycles"][0]
    assert sorted(cyc["path"][:-1]) == ["A", "B", "C"]
    assert cyc["vat_exposure_kobo"] == 215000
    assert sorted(cyc["invoice_ids"]) == ["i1", "i2", "i3"]


def test_i8_no_cycle_when_chain_open():
    r = client.post("/v1/insights/circularity", json={"invoices": INVS[:2]})
    assert r.json()["cycles"] == []


def test_i9_benchmarks_flags_2sigma_outlier():
    recs = [{"tin": f"T{i}", "sector": "retail", "turnover_kobo": 100_000_000,
             "tax_paid_kobo": t} for i, t in
            enumerate([7_500_000, 7_400_000, 7_600_000, 7_550_000, 7_450_000, 100_000])]
    body = client.post("/v1/insights/benchmarks", json={"records": recs}).json()
    assert body["sectors"]["retail"]["taxpayers"] == 6
    assert len(body["outliers"]) == 1
    assert body["outliers"][0]["direction"] == "below"
    assert body["outliers"][0]["tin"] == "T5"


def test_i10_penalty_late_filing_and_interest():
    body = client.post("/v1/insights/penalties", json={
        "tax_type": "VAT", "due_date": "2026-02-21",
        "filed_date": "2026-05-10", "paid_date": "2026-05-10",
        "tax_kobo": 100_000_000}).json()
    assert body["months_late_filing"] == 3
    assert body["late_filing_kobo"] == 10_000_000 + 2 * 5_000_000
    assert body["late_payment_kobo"] == 100_000_000 * 1000 // 10_000
    assert body["interest_kobo"] > 0
    assert body["total_kobo"] == (body["late_filing_kobo"] + body["late_payment_kobo"]
                                  + body["interest_kobo"])
    assert body["mpr_bps_used"] == 2700


def test_i10_no_penalty_when_on_time():
    body = client.post("/v1/insights/penalties", json={
        "tax_type": "CIT", "due_date": "2026-06-30", "filed_date": "2026-06-30",
        "paid_date": "2026-06-30", "tax_kobo": 50_000_000}).json()
    assert body["total_kobo"] == 0


def test_i10_wht_late_payment_is_10pct_gazette_aligned():
    # Audit fix: WHT failure-to-deduct/late-payment was 40% ("NTAA s.74",
    # wrong section); gazette-aligned NTAA s.65 rate is 10% + MPR interest.
    body = client.post("/v1/insights/penalties", json={
        "tax_type": "WHT", "due_date": "2026-02-21",
        "paid_date": "2026-03-21", "tax_kobo": 10_000_000}).json()
    assert body["late_payment_kobo"] == 10_000_000 * 1000 // 10_000
    assert body["interest_kobo"] > 0


def test_i10_registration_penalty_tiers():
    # NTAA s.100(1): N50k first month + N25k per subsequent month
    body = client.post("/v1/insights/penalties/registration", json={
        "months_unregistered": 3, "as_of": "2026-03-01"}).json()
    assert body["failure_to_register_kobo"] == 5_000_000 + 2 * 2_500_000
    # NTAA s.100(2): N5m per unregistered-person engagement
    body2 = client.post("/v1/insights/penalties/registration", json={
        "unregistered_contract_engagements": 2, "as_of": "2026-03-01"}).json()
    assert body2["unregistered_contract_kobo"] == 2 * 500_000_000_00
    # fail-closed before NTAA effective date
    r = client.post("/v1/insights/penalties/registration", json={
        "months_unregistered": 1, "as_of": "2025-06-01"})
    assert r.status_code == 422


def test_i11_reminder_uses_pack_calendar_and_history():
    body = client.post("/v1/insights/reminders", json={
        "tenant_id": "t1", "tax": "VAT", "period": "2026-02-15"}).json()
    assert body["reminder"]["deadline"] == "2026-03-21"
    assert body["reminder"]["lead_days"] == 3
    assert body["event"]["type"] == "nrs.reminders.due.v1"
    # chronic late filer -> 7-day lead
    body2 = client.post("/v1/insights/reminders", json={
        "tenant_id": "t1", "tax": "VAT", "period": "2026-02-15",
        "history": [{"filed_late": True}, {"filed_late": True}]}).json()
    assert body2["reminder"]["lead_days"] == 7
    assert body2["reminder"]["remind_on"] == "2026-03-14"
    # CIT annual: 6 months after year end
    body3 = client.post("/v1/insights/reminders", json={
        "tenant_id": "t1", "tax": "CIT", "period": "2026-01-01",
        "year_end": "2025-12-31"}).json()
    assert body3["reminder"]["deadline"] == "2026-06-30"


def test_i12_explain_card_ranks_fatal_first():
    trace = [
        {"rule_id": "br-co-15", "matched": True, "violation": "x", "severity": "warn",
         "narrate": "totals inconsistent"},
        {"rule_id": "br-nrs-01", "matched": True, "violation": "y", "severity": "fatal",
         "narrate": "supplier TIN missing"},
        {"rule_id": "br-01", "matched": True},
    ]
    card = client.post("/v1/insights/explain",
                       json={"invoice_id": "inv-1", "trace": trace}).json()
    assert card["headline"].startswith("BLOCKED")
    assert card["reasons"][0]["code"] == "br-nrs-01"
    assert card["model_score"] is None  # feature-store SIMULATED in dev


def test_i14_fx_convert_and_audit():
    body = client.post("/v1/insights/fx/convert", json={
        "amount_minor": 100_00, "currency": "USD", "date": "2026-03-01",
        "invoice_id": "inv-9"}).json()
    assert body["ngn_kobo"] == 15_500_000  # $100.00 @ N1,550.00
    assert body["source"] == "static-table"
    assert len(body["digest"]) == 16
    audit = client.get("/v1/insights/fx/audit").json()
    assert audit["count"] == 1
    bad = client.post("/v1/insights/fx/convert", json={
        "amount_minor": 100, "currency": "JPY", "date": "2026-03-01"})
    assert bad.status_code == 422
