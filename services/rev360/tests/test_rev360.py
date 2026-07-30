import pytest
from fastapi.testclient import TestClient

from app.main import app
from app import core

client = TestClient(app)
H = {"X-Dev-Role": "operator"}

CSV = """record_id,tin,taxpayer_name,tax_type,period,amount_kobo,payment_ref,payment_kobo,assessment_ref,tcc_ref,record_date
r1,1234567890123,Acme Ltd,CIT,2025-12,500000000,PAY-001,500000000,ASM-1,TCC-1,2026-01-05
r2,1234567890123,Acme Ltd,VAT,2025-12,75000000,PAY-002,75000000,ASM-2,,2026-01-06
r3,9876543210987,Beta Enterprises,PIT,2025-12,120000000,PAY-003,120000000,ASM-3,TCC-3,2026-01-07
r4,5555555555555,Gamma Traders,WHT,2025-11,30000000,PAY-002,75000000,ASM-4,,2026-01-08
r5,5555555555555,Gamma Traders,VAT,2025-11,45000000,,,ASM-5,,2026-01-09
"""

XML = """<ledger>
  <record><record_id>x1</record_id><tin>7777777777777</tin>
    <taxpayer_name>Delta Co</taxpayer_name><tax_type>CIT</tax_type>
    <period>2025-10</period><amount_kobo>900000000</amount_kobo>
    <payment_ref>PAY-X1</payment_ref><payment_kobo>900000000</payment_kobo>
    <assessment_ref>ASM-X1</assessment_ref><tcc_ref>TCC-X1</tcc_ref>
    <record_date>2025-11-02</record_date></record>
</ledger>"""


def test_health():
    r = client.get("/healthz")
    assert r.status_code == 200 and r.json()["service"] == "rev360"
    assert client.get("/readyz").status_code == 200


def test_auth_required():
    assert client.post("/v1/ingest", content=CSV).status_code == 401
    # dev SSO issues a usable token
    r = client.get("/v1/sso/login")
    token = r.json()["token"]
    ok = client.get("/v1/records", headers={"Authorization": f"Bearer {token}"})
    assert ok.status_code == 200


def test_ingest_csv_and_xml():
    r = client.post("/v1/ingest", content=CSV,
                    headers={**H, "Content-Type": "text/csv"})
    assert r.status_code == 201 and r.json()["ingested"] == 5
    r2 = client.post("/v1/ingest", content=XML,
                     headers={**H, "Content-Type": "application/xml"})
    assert r2.status_code == 201 and r2.json()["format"] == "xml"
    recs = client.get("/v1/records", headers=H).json()
    assert recs["count"] == 6


def test_ingest_bad_csv():
    r = client.post("/v1/ingest", content="a,b\n1,2\n", headers=H)
    assert r.status_code == 422
    assert r.headers["content-type"].startswith("application/problem+json")


def test_reconcile_detects_defect_classes():
    client.post("/v1/ingest", content=CSV, headers=H)
    run = client.post("/v1/reconcile/run", headers=H)
    assert run.status_code == 201
    summary = run.json()
    assert summary["records"] >= 5
    # duplicate payment ref PAY-002 in r2+r4 must be caught deterministically
    defects = client.get("/v1/defects", headers=H).json()["defects"]
    classes = {d["defect_class"] for d in defects}
    assert "duplicate_payment" in classes
    dup = [d for d in defects if d["defect_class"] == "duplicate_payment"][0]
    assert dup["detail"]["payment_ref"] == "PAY-002"
    assert dup["severity"] == "medium"
    # taxonomy is fully exposed
    tax = client.get("/v1/defects", headers=H).json()["taxonomy"]
    assert set(tax) == {"wrong_assessment", "blocked_tcc",
                        "unrecognised_remittance", "duplicate_payment",
                        "tin_mismatch"}
    # simulator produces at least one NRS-side discrepancy across fixtures
    assert len(defects) >= 2


def test_severity_rubric():
    assert core.severity_for("wrong_assessment", {
        "legacy_amount_kobo": 100, "rev360_amount_kobo": 300}) == "critical"
    assert core.severity_for("wrong_assessment", {
        "legacy_amount_kobo": 100, "rev360_amount_kobo": 120}) == "high"
    assert core.severity_for("wrong_assessment", {
        "legacy_amount_kobo": 100, "rev360_amount_kobo": 105}) == "medium"
    assert core.severity_for("unrecognised_remittance", {
        "amount_kobo": 20_000_000_00}) == "critical"
    assert core.severity_for("tin_mismatch", {}) == "low"


def test_case_crud_and_resolve_with_worm():
    client.post("/v1/ingest", content=CSV, headers=H)
    run = client.post("/v1/reconcile/run", headers=H).json()
    assert run["defects"] >= 1
    defect = client.get("/v1/defects", headers=H).json()["defects"][0]

    c = client.post("/v1/cases", headers=H, json={
        "title": "Fix duplicate payment", "defect_id": defect["id"],
        "assignee": "consultant@meridian.local", "priority": "high"})
    assert c.status_code == 201
    cid = c.json()["id"]

    p = client.patch(f"/v1/cases/{cid}", headers=H,
                     json={"status": "in_progress", "notes": "investigating"})
    assert p.json()["status"] == "in_progress"

    res = client.post(f"/v1/cases/{cid}/resolve", headers=H, json={
        "correction": {"field": "credited_kobo", "new_value": 75000000},
        "note": "duplicate reversed"})
    assert res.status_code == 200
    body = res.json()
    assert body["case"]["status"] == "resolved"
    ev = body["evidence"]
    assert ev["sha256"] and ev["id"].startswith("ev-")
    # WORM evidence verifies
    v = client.get(f"/v1/evidence/{ev['id']}/verify", headers=H).json()
    assert v["found"] and v["valid"]
    # defect marked corrected
    d = client.get(f"/v1/defects?status=corrected", headers=H).json()
    assert any(x["id"] == defect["id"] for x in d["defects"])
    # double resolve rejected
    again = client.post(f"/v1/cases/{cid}/resolve", headers=H,
                        json={"correction": {}})
    assert again.status_code == 409


def test_etl_endpoints():
    client.post("/v1/ingest", content=CSV, headers=H)
    ex = client.post("/v1/etl/extract", headers=H)
    assert ex.status_code == 201 and ex.json()["staged"] >= 5
    ld = client.post("/v1/etl/load", headers=H)
    assert ld.status_code == 200 and ld.json()["loaded"] >= 5
