"""I2 TaxProMax CSV export tests: auth required, TIN scoping, CSV content
(row mapping + kobo->naira 2dp), period range filter, empty result."""
import csv
import io

import jwt as pyjwt
from fastapi.testclient import TestClient

from app.main import app
from app.taxpromax import kobo_to_naira

client = TestClient(app)

DEV_SECRET = "meridian-dev-secret-change-me-32!"
OPERATOR = {"X-Dev-Role": "operator"}
AUDITOR = {"X-Dev-Role": "auditor"}
TIN = "TIN-I2-EXP"


def _taxpayer(tin: str) -> dict:
    tok = pyjwt.encode({"sub": f"tp-{tin}", "roles": ["taxpayer"],
                        "tin": tin}, DEV_SECRET, algorithm="HS256")
    return {"Authorization": f"Bearer {tok}"}


def _file_vat(period: str, net_kobo: int, vat_kobo: int,
              tin: str = TIN) -> dict:
    resp = client.post("/v1/filings/vat", headers=OPERATOR, json={
        "tin": tin, "period": period,
        "idempotency_key": f"i2-{tin}-{period}",
        "invoices": [{"irn": f"I-{tin}-{period}", "direction": "sale",
                      "basket": "standard_75",
                      "net_kobo": net_kobo, "vat_kobo": vat_kobo}]})
    assert resp.status_code == 201, resp.text
    return resp.json()


def _file_paye(period: str, tin: str = TIN) -> dict:
    resp = client.post("/v1/filings/paye", headers=OPERATOR, json={
        "employer_tin": tin, "period": period,
        "idempotency_key": f"i2-paye-{tin}-{period}",
        "employees": [{"tin": "EMP-1", "name": "Ada Lovelace",
                       "gross_kobo": 500_000_00}]})
    assert resp.status_code == 201, resp.text
    return resp.json()


def _export(params: str, headers: dict) -> tuple[int, list[list[str]]]:
    resp = client.get(f"/v1/exports/taxpromax.csv?{params}", headers=headers)
    if resp.status_code != 200:
        return resp.status_code, []
    rows = list(csv.reader(io.StringIO(resp.text)))
    return 200, rows


# ---------- unit: kobo -> naira ----------

def test_kobo_to_naira_exactly_2dp():
    assert kobo_to_naira(0) == "0.00"
    assert kobo_to_naira(450_000_00) == "450000.00"
    assert kobo_to_naira(7) == "0.07"
    assert kobo_to_naira(1_234_567_89) == "1234567.89"
    assert kobo_to_naira(100_050) == "1000.50"


# ---------- auth ----------

def test_anonymous_export_denied():
    assert client.get(f"/v1/exports/taxpromax.csv?tin={TIN}").status_code == 401


def test_cross_tenant_taxpayer_denied():
    assert _export(f"tin={TIN}", _taxpayer("TIN-I2-OTHER"))[0] == 403


def test_own_tin_taxpayer_allowed():
    assert _export(f"tin={TIN}", _taxpayer(TIN))[0] == 200
    assert _export(f"tin={TIN}", AUDITOR)[0] == 200


# ---------- content ----------

def test_csv_row_mapping_and_naira_formatting():
    vat_rec = _file_vat("2026-04", 10_000_000_00, 750_000_00)
    paye_rec = _file_paye("2026-04")
    status, rows = _export(f"tin={TIN}&from_period=2026-04&to_period=2026-04",
                           OPERATOR)
    assert status == 200
    assert rows[0] == ["TIN", "Taxpayer Name", "Period", "Tax Type",
                       "Taxable Amount", "Tax Amount", "Net Payable",
                       "Currency", "Filing Reference", "Status"]
    paye_row = next(r for r in rows[1:] if r[3] == "PAYE")
    vat_row = next(r for r in rows[1:] if r[3] == "VAT")
    assert paye_row[:4] == [TIN, "", "2026-04", "PAYE"]
    assert paye_row[4:7] == ["500000.00", paye_rec and
                             f"{paye_rec['totals']['tax_kobo'] / 100:.2f}",
                             f"{paye_rec['totals']['tax_kobo'] / 100:.2f}"]
    assert paye_row[7:10] == ["NGN", paye_rec["return_id"], "filed"]
    assert vat_row[:4] == [TIN, "", "2026-04", "VAT"]
    assert vat_row[4:7] == ["10000000.00", "750000.00",
                            f"{vat_rec['net_vat_payable_kobo'] / 100:.2f}"]
    assert vat_row[7:10] == ["NGN", vat_rec["return_id"], "filed"]


# ---------- period filter ----------

def test_period_range_filter():
    _file_vat("2026-01", 100_000_00, 7_500_00, tin="TIN-I2-RANGE")
    _file_vat("2026-02", 200_000_00, 15_000_00, tin="TIN-I2-RANGE")
    _file_vat("2026-03", 300_000_00, 22_500_00, tin="TIN-I2-RANGE")
    _, rows = _export("tin=TIN-I2-RANGE&from_period=2026-02&to_period=2026-03",
                      OPERATOR)
    periods = [r[2] for r in rows[1:]]
    assert periods == ["2026-02", "2026-03"]
    _, rows = _export("tin=TIN-I2-RANGE", OPERATOR)
    assert [r[2] for r in rows[1:]] == ["2026-01", "2026-02", "2026-03"]


def test_tax_type_filter():
    _, rows = _export(f"tin={TIN}&tax_type=PAYE", OPERATOR)
    assert rows[1:] and all(r[3] == "PAYE" for r in rows[1:])
    assert _export(f"tin={TIN}&tax_type=BOGUS", OPERATOR)[0] == 400


# ---------- empty result -> headers-only ----------

def test_empty_result_headers_only():
    status, rows = _export("tin=TIN-I2-EMPTY", OPERATOR)
    assert status == 200
    assert len(rows) == 1
    assert rows[0][0] == "TIN" and rows[0][-1] == "Status"


# ---------- audit ----------

def test_export_is_audit_logged():
    from app.main import vat_store
    before = len(vat_store._docs.scan("export_audit"))
    _export("tin=TIN-I2-AUDIT", OPERATOR)
    after = vat_store._docs.scan("export_audit")
    assert len(after) == before + 1
    entry = after[-1]
    assert entry["event"] == "taxpromax_export"
    assert entry["tin"] == "TIN-I2-AUDIT"
    assert entry["principal"]
