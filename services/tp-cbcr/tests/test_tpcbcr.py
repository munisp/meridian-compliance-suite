import xml.etree.ElementTree as ET

from fastapi.testclient import TestClient

from app.main import app

client = TestClient(app)
H = {"X-Dev-Role": "operator"}


def seed_graph():
    client.post("/v1/graph/entities", headers=H, json={
        "tin": "1000000000001", "name": "HoldCo PLC",
        "jurisdiction": "NG", "role": "ultimate_parent"})
    client.post("/v1/graph/entities", headers=H, json={
        "tin": "1000000000002", "name": "SubCo Ltd",
        "jurisdiction": "NG", "role": "subsidiary"})
    client.post("/v1/graph/transactions", headers=H, json={
        "from_tin": "1000000000001", "to_tin": "1000000000002",
        "tx_type": "loan", "amount_kobo": 500_000_000_00})
    client.post("/v1/graph/transactions", headers=H, json={
        "from_tin": "1000000000002", "to_tin": "1000000000001",
        "tx_type": "interest", "amount_kobo": 50_000_000_00})


def test_health_ready():
    assert client.get("/healthz").json()["service"] == "tp-cbcr"
    assert client.get("/readyz").json()["baseline_pack"] == "rp-tp-2018@1.0.0"


def test_graph_ingest():
    seed_graph()
    ents = client.get("/v1/graph/entities", headers=H).json()
    assert ents["count"] >= 2
    txs = client.get("/v1/graph/transactions", headers=H).json()
    assert txs["count"] >= 2
    assert txs["controlled_total_kobo"] >= 550_000_000_00


CBCR_PAYLOAD = {
    "reporting_period": "2025-12-31",
    "reporting_entity": {"tin": "1000000000001", "name": "HoldCo PLC",
                         "jurisdiction": "NG"},
    "jurisdictions": [
        {"country": "NG",
         "revenue_unrelated_kobo": 120_000_000_000_00,
         "revenue_related_kobo": 45_000_000_000_00,
         "profit_or_loss_kobo": 30_000_000_000_00,
         "tax_paid_kobo": 8_000_000_000_00,
         "tax_accrued_kobo": 9_000_000_000_00,
         "capital_kobo": 200_000_000_000_00,
         "earnings_kobo": 60_000_000_000_00,
         "employees": 450,
         "assets_kobo": 500_000_000_000_00,
         "constituent_entities": [
             {"tin": "1000000000001", "name": "HoldCo PLC"},
             {"tin": "1000000000002", "name": "SubCo Ltd"}]},
        {"country": "GH",
         "revenue_unrelated_kobo": 5_000_000_000_00,
         "revenue_related_kobo": 1_000_000_000_00,
         "profit_or_loss_kobo": 900_000_000_00,
         "tax_paid_kobo": 200_000_000_00, "tax_accrued_kobo": 250_000_000_00,
         "capital_kobo": 3_000_000_000_00, "earnings_kobo": 800_000_000_00,
         "employees": 30, "assets_kobo": 4_000_000_000_00,
         "constituent_entities": [{"tin": "GH-1", "name": "Ghana Sub"}]},
    ],
}


def test_cbcr_xml_generation():
    r = client.post("/v1/cbcr/generate", headers=H, json=CBCR_PAYLOAD)
    assert r.status_code == 201
    body = r.json()
    # group revenue N171bn >= N160bn -> CbCR required per rp-tp-2018
    assert body["cbcr_required"] is True
    xml_text = body["xml"]
    root = ET.fromstring(xml_text)
    ns = {"c": "urn:oecd:ties:cbc:v2"}
    assert root.tag == "{urn:oecd:ties:cbc:v2}CbC_OECD"
    assert root.find("c:MessageSpec/c:MessageType", ns).text == "CBC"
    assert root.find("c:MessageSpec/c:MessageTypeIndic", ns).text == "CBC401"
    reports = root.findall("c:CbCBody/c:CbCReports/c:CbCReport", ns)
    assert len(reports) == 2
    ng = reports[0]
    total = ng.find("c:Summary/c:Revenue/c:Total", ns)
    # N165bn whole units
    assert total.text == str((120_000_000_000_00 + 45_000_000_000_00) // 100)
    assert total.get("currCode") == "NGN"
    consts = ng.findall("c:ConstEntities/c:ConstEntity", ns)
    assert len(consts) == 2
    # raw XML format also available
    r2 = client.post("/v1/cbcr/generate?format=xml", headers=H,
                     json=CBCR_PAYLOAD)
    assert r2.headers["content-type"].startswith("application/xml")


def test_master_and_local_file():
    seed_graph()
    mf = client.post("/v1/docs/master-file", headers=H, json={
        "group": {"name": "HoldCo Group", "ultimate_parent_tin": "1000000000001",
                  "reporting_period": "2025-12-31"}})
    assert mf.status_code == 201
    doc = mf.json()
    assert doc["document"] == "master_file"
    assert set(doc["sections"]) == {"group_structure", "business_description",
                                    "intangibles", "financing", "financial_tax"}
    html_r = client.post("/v1/docs/master-file?format=html", headers=H,
                         json={"group": {"name": "HoldCo Group"}})
    assert "text/html" in html_r.headers["content-type"]
    assert "Master File" in html_r.text

    lf = client.post("/v1/docs/local-file", headers=H, json={
        "entity": {"tin": "1000000000002", "name": "SubCo Ltd"},
        "financials": {"revenue_kobo": 80_000_000_000_00,
                       "ebitda_kobo": 20_000_000_000_00,
                       "tp_method": "CUP"}})
    assert lf.status_code == 201
    ldoc = lf.json()
    assert ldoc["document"] == "local_file"
    cts = ldoc["sections"]["controlled_transactions"]
    assert cts["total_kobo"] >= 50_000_000_00
    assert "interest" in cts["totals_by_type_kobo"]


def test_interest_deductibility():
    # EBITDA N200m -> limit 30% = N60m; interest N80m -> disallow N20m
    r = client.post("/v1/interest/deductibility", headers=H, json={
        "ebitda_kobo": 200_000_000_00, "interest_kobo": 80_000_000_00,
        "has_connected_party_debt": True})
    body = r.json()
    assert body["limit_bps_of_ebitda"] == 3000
    assert body["limit_kobo"] == 60_000_000_00
    assert body["deductible_kobo"] == 60_000_000_00
    assert body["disallowed_kobo"] == 20_000_000_00
    assert body["carryforward_out_kobo"] == 20_000_000_00
    assert body["max_carryforward_years"] == 5
    # carryforward expiry after 5 years
    r2 = client.post("/v1/interest/deductibility", headers=H, json={
        "ebitda_kobo": 100_000_000_00, "interest_kobo": 50_000_000_00,
        "carryforward_years_used": 5, "has_connected_party_debt": True})
    assert r2.json()["carryforward_expired_kobo"] == \
        r2.json()["disallowed_kobo"]


def test_pack_pin_mechanism():
    r = client.put("/v1/packs/pin/tenant-a", headers=H,
                   json={"pack_id": "rp-tp-2018", "version": "1.0.0"})
    assert r.status_code == 200
    pins = client.get("/v1/packs/pins", headers=H).json()["pins"]
    assert any(p["tenant_id"] == "tenant-a" for p in pins)
    # invalid version rejected
    bad = client.put("/v1/packs/pin/tenant-b", headers=H,
                     json={"pack_id": "rp-tp-2018", "version": "9.9.9"})
    assert bad.status_code in (404, 500)


def test_fx():
    fx = client.get("/v1/fx", headers=H).json()
    assert fx["base"] == "NGN"
    conv = client.post("/v1/fx/convert", headers=H, json={
        "amount_minor": 155_000_00, "from": "NGN", "to": "USD"})
    assert conv.json()["converted_minor"] == 100_00  # N1550 -> $1
    up = client.post("/v1/fx/rates", headers=H, json={
        "currency": "USD", "per_ngn": 1600.0, "as_of": "2026-02-01"})
    assert up.status_code == 201
    conv2 = client.post("/v1/fx/convert", headers=H, json={
        "amount_minor": 160_000_00, "from": "NGN", "to": "USD"})
    assert conv2.json()["converted_minor"] == 100_00
