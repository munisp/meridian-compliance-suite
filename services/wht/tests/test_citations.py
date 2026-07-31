"""LCE SPEC §5 runtime citation chain — wht evaluate responses carry statute
citations resolved [REAL] from the coverage matrix reverse index.
citation_kind stays "secondary" until CTC verification (registry workstream).
"""
from fastapi.testclient import TestClient

from app.main import app

client = TestClient(app)
H = {"X-Dev-Role": "operator"}


def _by_rule(body, rule_id):
    for c in body["citations"]:
        if c["rule_id"] == rule_id:
            return c
    raise AssertionError(f"no citation for {rule_id} in {body['citations']}")


def test_directors_fees_15pct_cites_first_schedule():
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "directors_fees", "beneficiary": "individual",
        "amount_kobo": 2_000_000_00, "supplier_tin": "1234567890123",
        "payment_date": "2026-02-10"})
    body = r.json()
    assert r.status_code == 200 and body["rate_bps"] == 1500
    cit = _by_rule(body, "wht.rate.directors-fees.individual")
    assert "WHT Regs 2024, First Schedule" in cit["citation"]
    assert cit["statute"] == "wht-regs-2024"
    assert cit["section_id"] == "first-schedule.directors-fees"
    assert cit["statute_sections"] == [
        "wht-regs-2024:first-schedule.directors-fees"]
    assert cit["citation_kind"] == "secondary"  # until CTC verification
    assert cit["pack_id"] == "rp-wht-2024" and cit["pack_version"] == "1.0.0"
    # every computed amount maps back to the rules that priced it
    assert "wht.rate.directors-fees.individual" in \
        body["amount_citations"]["wht_kobo"]
    assert body["amount_citations"]["net_payable_kobo"] == \
        body["amount_citations"]["wht_kobo"]


def test_passive_income_no_tin_not_doubled_cites_regs():
    # dividends are passive income: no-TIN double rate does NOT apply (reg. 8);
    # the response still cites the Regs for the 10% First Schedule rate.
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "dividend", "beneficiary": "company",
        "amount_kobo": 10_000_000_00, "payment_date": "2026-02-10"})
    body = r.json()
    assert body["rate_bps"] == 1000
    assert body["no_tin_double_applied"] is False
    cit = _by_rule(body, "wht.rate.dividend.company")
    assert cit["statute"] == "wht-regs-2024"
    assert cit["section_id"] == "first-schedule.dividend"
    # pack rule carries no short citation -> fallback composes statute+section
    assert "Withholding) Regulations 2024" in cit["citation"]
    assert cit["citation_kind"] == "secondary"
    # the no-TIN double-rate rule must NOT be in the matched set for dividends
    assert "wht.no-tin.double-rate" not in body["matched_rules"]


def test_active_income_no_tin_double_cites_reg_8():
    r = client.post("/v1/wht/evaluate", headers=H, json={
        "payment_type": "services", "beneficiary": "company",
        "amount_kobo": 5_000_000_00, "payment_date": "2026-02-10"})
    body = r.json()
    assert body["rate_bps"] == 400 and body["no_tin_double_applied"] is True
    cit = _by_rule(body, "wht.no-tin.double-rate")
    assert cit["section_id"] == "reg-8.no-tin-double-rate"
    assert "wht-regs-2024:reg-8.no-tin-double-rate" in cit["statute_sections"]
    assert "wht.no-tin.double-rate" in body["amount_citations"]["wht_kobo"]


def test_citations_never_break_evaluation():
    # unknown-to-coverage rules still return a citation shell (SPEC §5.1 rule b)
    from meridian_py.citations import CitationResolver
    res = CitationResolver("/nonexistent/coverage-dir")
    cit = res.resolve("rp-wht-2024", "1.0.0", "wht.rate.rent.company")
    assert cit["statute_sections"] == [] and cit["rule_id"] == "wht.rate.rent.company"
