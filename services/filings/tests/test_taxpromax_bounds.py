"""Regression (R4 S3#11): the TaxProMax export previously scanned ALL
tenants' vat_returns/paye_returns and materialised an unbounded row list
per request (DoS amplifier). The per-tenant filter is now pushed into the
store query and the export is bounded at EXPORT_MAX_ROWS."""
import csv
import io

import jwt as pyjwt
from fastapi.testclient import TestClient

import app.main as main
from app import store, taxpromax

client = TestClient(main.app)

DEV_SECRET = "meridian-dev-secret-change-me-32!"
OPERATOR = {"X-Dev-Role": "operator"}
TIN = "TIN-I2-CAP"


class _SpyDocs(store.DocStore):
    """Records query vs scan usage to prove the pushdown contract."""

    def __init__(self):
        super().__init__(dsn="")
        self.queries: list[tuple[str, dict, int]] = []
        self.scans: list[str] = []

    def query(self, collection, filters, limit):
        self.queries.append((collection, filters, limit))
        return super().query(collection, filters, limit)

    def scan(self, collection):
        self.scans.append(collection)
        return super().scan(collection)


class _Wrap:
    """Mimic the VatReturnStore/PayeReturnStore _docs surface."""

    def __init__(self, docs):
        self._docs = docs


def _vat_rec(tin, period, rid):
    return {"tin": tin, "period": period, "return_id": rid, "status": "filed",
            "sales_schedule": {"total_sales_kobo": 100_00},
            "output_vat_kobo": 7_50, "net_vat_payable_kobo": 7_50}


def test_collect_rows_pushes_tenant_filter_to_store():
    docs = _SpyDocs()
    docs.put("vat_returns", "r1", _vat_rec(TIN, "2026-01", "FR-1"))
    docs.put("vat_returns", "r2", _vat_rec("OTHER-TIN", "2026-01", "FR-2"))
    rows, truncated = taxpromax.collect_rows(_Wrap(docs), _Wrap(docs), TIN,
                                             tax_type="VAT")
    assert not truncated
    assert [r[8] for r in rows] == ["FR-1"]
    # The filter went to the store; no full-collection scan happened.
    assert docs.scans == []
    assert docs.queries == [("vat_returns", {"tin": TIN},
                            taxpromax.EXPORT_MAX_ROWS + 1)]


def test_collect_rows_bounded_and_truncated():
    docs = store.DocStore(dsn="")
    for i in range(5):
        docs.put("vat_returns", f"r{i}", _vat_rec(TIN, f"2026-0{i + 1}", f"FR-{i}"))
    rows, truncated = taxpromax.collect_rows(_Wrap(docs), _Wrap(docs), TIN,
                                             tax_type="VAT", max_rows=3)
    assert truncated is True
    assert len(rows) == 3
    # Not truncated when under the cap.
    rows, truncated = taxpromax.collect_rows(_Wrap(docs), _Wrap(docs), TIN,
                                             tax_type="VAT", max_rows=10)
    assert truncated is False
    assert len(rows) == 5


def test_export_endpoint_truncation_header():
    tin = "TIN-I2-TRUNC"
    for i in range(4):
        resp = client.post("/v1/filings/vat", headers=OPERATOR, json={
            "tin": tin, "period": f"2026-0{6 + i}",
            "idempotency_key": f"i2-trunc-{i}",
            "invoices": [{"irn": f"I-trunc-{i}", "direction": "sale",
                          "basket": "standard_75",
                          "net_kobo": 100_00, "vat_kobo": 7_50}]})
        assert resp.status_code == 201, resp.text
    old = taxpromax.EXPORT_MAX_ROWS
    taxpromax.EXPORT_MAX_ROWS = 2
    try:
        tok = pyjwt.encode({"sub": f"tp-{tin}", "roles": ["taxpayer"],
                            "tin": tin}, DEV_SECRET, algorithm="HS256")
        resp = client.get(f"/v1/exports/taxpromax.csv?tin={tin}&tax_type=VAT",
                          headers={"Authorization": f"Bearer {tok}"})
        assert resp.status_code == 200, resp.text
        assert resp.headers.get("X-Export-Truncated") == "true"
        rows = list(csv.reader(io.StringIO(resp.text)))
        assert len(rows) == 1 + 2  # header + capped rows
    finally:
        taxpromax.EXPORT_MAX_ROWS = old
