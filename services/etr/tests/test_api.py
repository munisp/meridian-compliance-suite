"""API tests: ingest, compute, trace, GIR XML, filing pack, workflows, auth."""
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
os.environ["ETR_DB"] = ":memory:"

from fastapi.testclient import TestClient

from app.main import app

client = TestClient(app)
H = {"X-Dev-Role": "operator"}


def seed():
    client.post("/v1/etr/groups", json={
        "id": "g1", "name": "API Test MNE", "consolidated_revenue_kobo": 20_000_000_000_000_000},
        headers=H)
    client.post("/v1/etr/entities", json=[
        {"id": "upe", "group_id": "g1", "name": "UPE", "jurisdiction": "NG", "is_upe": True,
         "net_income_kobo": 500_000_000_00, "covered_taxes_kobo": 150_000_000_00},
        {"id": "sub", "group_id": "g1", "name": "LowTax Sub", "jurisdiction": "MU",
         "parent_id": "upe", "net_income_kobo": 1_000_000_000_00,
         "covered_taxes_kobo": 100_000_000_00, "payroll_kobo": 10_000_000_00,
         "tangible_assets_kobo": 10_000_000_00},
    ], headers=H)


def test_health():
    r = client.get("/healthz")
    assert r.status_code == 200 and r.json()["service"] == "etr"


def test_auth_required():
    r = client.get("/v1/etr/entities")
    assert r.status_code == 401
    assert r.headers["content-type"].startswith("application/problem+json")


def test_dev_token_flow():
    r = client.post("/v1/dev-token", json={"sub": "tester", "roles": ["operator"]})
    token = r.json()["token"]
    r2 = client.get("/v1/packs", headers={"Authorization": f"Bearer {token}"})
    assert r2.status_code == 200
    assert len(r2.json()["packs"]) == 5


def test_full_flow():
    seed()
    r = client.post("/v1/etr/compute", json={"group_id": "g1", "fiscal_year": 2025, "basis": "dual"}, headers=H)
    assert r.status_code == 201, r.text
    comp = r.json()
    assert comp["in_scope"] is True
    assert comp["total_topup_kobo"] > 0
    cid = comp["id"]
    # trace
    tr = client.get(f"/v1/etr/computations/{cid}/trace", headers=H)
    assert tr.status_code == 200 and len(tr.json()["steps"]) >= 6
    # GIR XML
    gir = client.get(f"/v1/etr/computations/{cid}/gir.xml", headers=H)
    assert gir.status_code == 200
    assert "urn:oecd:ties:globe:gir:v1" in gir.text
    assert "JurisdictionETR" in gir.text and "TopupAllocation" in gir.text
    # filing pack
    fp = client.get(f"/v1/etr/computations/{cid}/filing-pack.json", headers=H)
    body = fp.json()
    assert body["filing_pack_digest"]
    assert body["entity_roster"] and body["step_trace"] and body["pack_versions"]
    # list
    lst = client.get("/v1/etr/computations?group_id=g1", headers=H)
    assert any(c["id"] == cid for c in lst.json()["computations"])


def test_workflows():
    seed()
    r = client.post("/v1/workflows/wf-etr-compute/run",
                    json={"group_id": "g1", "fiscal_year": 2026}, headers=H)
    assert r.status_code == 200
    body = r.json()
    assert body["status"] == "completed"
    assert body["computation"]["fiscal_year"] == 2026
    r2 = client.post("/v1/workflows/wf-globe-extract/run", json={"fiscal_year": 2026}, headers=H)
    assert r2.json()["result"]["sbie"] == {"payroll_bps": 940, "assets_bps": 740}
    cid = body["computation"]["id"]
    r3 = client.post("/v1/workflows/wf-filingpack-build/run",
                     json={"computation_id": cid}, headers=H)
    assert r3.status_code == 200 and "gir_xml" in r3.json()
    r4 = client.post("/v1/workflows/wf-nope/run", json={}, headers=H)
    assert r4.status_code == 404


def _seed_lowtax_ng(gid: str):
    client.post("/v1/etr/groups", json={
        "id": gid, "name": "Gate Test MNE", "consolidated_revenue_kobo": 20_000_000_000_000_000},
        headers=H)
    client.post("/v1/etr/entities", json=[
        {"id": f"{gid}-e", "group_id": gid, "name": "NG LowTax", "jurisdiction": "NG",
         "net_income_kobo": 1_000_000_000_00, "covered_taxes_kobo": 50_000_000_00},
    ], headers=H)


def test_compute_ignores_client_qdmtt_flag_gate_closed(monkeypatch):
    """B2-#10: client qdmtt_upgrade=True + server gate disarmed -> no QDMTT."""
    import app.main as m
    monkeypatch.setattr(m, "qdmtt_upgrade_armed", lambda: (False, "default"))
    _seed_lowtax_ng("g-gate-off")
    r = client.post("/v1/etr/compute",
                    json={"group_id": "g-gate-off", "fiscal_year": 2025, "qdmtt_upgrade": True},
                    headers=H)
    assert r.status_code == 201, r.text
    comp = r.json()
    ng = comp["jurisdictions"][0]
    assert ng["topup_kobo"] > 0
    assert ng["qdmtt_applied"] is False and ng["residual_topup_kobo"] == ng["topup_kobo"]
    assert comp["qdmtt_upgrade"] is False


def test_compute_server_gate_armed_applies_qdmtt(monkeypatch):
    """B2-#10: server gate armed -> QDMTT even when client field is False."""
    import app.main as m
    monkeypatch.setattr(m, "qdmtt_upgrade_armed", lambda: (True, "reg-watch"))
    _seed_lowtax_ng("g-gate-on")
    r = client.post("/v1/etr/compute",
                    json={"group_id": "g-gate-on", "fiscal_year": 2025, "qdmtt_upgrade": False},
                    headers=H)
    assert r.status_code == 201, r.text
    ng = r.json()["jurisdictions"][0]
    assert ng["qdmtt_applied"] is True and ng["qdmtt_kobo"] == ng["topup_kobo"]
    assert ng["residual_topup_kobo"] == 0
