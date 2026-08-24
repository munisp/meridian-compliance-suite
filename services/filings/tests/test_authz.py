"""B2 regression (finding #1): the filings service had NO authentication on
any route — anonymous callers could file VAT/PAYE/CIT, issue statutory
assessments, and decide objections for any TIN.

Post-fix contract:
- every route requires a verified principal (401 anonymous)
- filing routes require taxpayer/operator/admin and TIN ownership for
  taxpayer principals (403 cross-TIN)
- assessment issue / objection decision / tick are officer-only
  (operator|admin); taxpayers get 403
"""
import jwt as pyjwt
import pytest
from fastapi.testclient import TestClient

from app.main import app

client = TestClient(app)

DEV_SECRET = "meridian-dev-secret-change-me-32!"

OFFICER = {"X-Dev-Role": "operator"}
ADMIN = {"X-Dev-Role": "admin"}
AUDITOR = {"X-Dev-Role": "auditor"}


def _taxpayer(tin: str) -> dict:
    tok = pyjwt.encode({"sub": f"tp-{tin}", "roles": ["taxpayer"],
                        "tin": tin}, DEV_SECRET, algorithm="HS256")
    return {"Authorization": f"Bearer {tok}"}


def _vat_body(tin: str = "TIN-AUTHZ") -> dict:
    return {"tin": tin, "period": "2026-03",
            "idempotency_key": "authz-" + tin,
            "invoices": [{"invoice_id": "inv-1", "supply_type": "goods",
                          "net_kobo": 100_000, "vat_kobo": 7_500}]}


def _asm_body(tin: str = "TIN-AUTHZ") -> dict:
    return {"tin": tin, "tax_type": "VAT", "period": "2026-03",
            "kind": "additional", "amount_kobo": 500_000,
            "grounds": "desk review", "served_via": "registered_post",
            "served_at": "2026-04-01"}


# ---- anonymous: every route 401 (pre-fix: all succeeded) ----

def test_anonymous_file_vat_denied():
    assert client.post("/v1/filings/vat", json=_vat_body()).status_code == 401


def test_anonymous_get_vat_denied():
    assert client.get("/v1/filings/vat/TIN-AUTHZ/2026-03").status_code == 401


def test_anonymous_file_paye_denied():
    body = {"employer_tin": "EMP-A", "period": "2026-03",
            "idempotency_key": "authz-paye", "employees": []}
    assert client.post("/v1/filings/paye", json=body).status_code == 401


def test_anonymous_cit_compute_denied():
    body = {"tin": "TIN-AUTHZ", "fye": "2025-12-31",
            "assessable_profit_kobo": 1, "turnover_kobo": 1,
            "total_fixed_assets_kobo": 1}
    assert client.post("/v1/filings/cit/compute", json=body).status_code == 401


def test_anonymous_issue_assessment_denied():
    assert client.post("/v1/assessments", json=_asm_body()).status_code == 401


def test_anonymous_decide_objection_denied():
    assert client.post("/v1/objections/OBJ-000001/decision",
                       json={"outcome": "upheld",
                             "decided_at": "2026-05-01"}).status_code == 401


# ---- role checks ----

def test_taxpayer_cannot_issue_assessment():
    resp = client.post("/v1/assessments", json=_asm_body(),
                       headers=_taxpayer("TIN-AUTHZ"))
    assert resp.status_code == 403


def test_auditor_cannot_issue_assessment():
    resp = client.post("/v1/assessments", json=_asm_body(), headers=AUDITOR)
    assert resp.status_code == 403


def test_officer_issues_assessment_and_decides():
    r = client.post("/v1/assessments", json=_asm_body(), headers=OFFICER)
    assert r.status_code == 201, r.text
    aid = r.json()["assessment_id"]
    obj = client.post(f"/v1/assessments/{aid}/objections",
                      json={"grounds": "wrong base", "admitted_amount_kobo": 0,
                            "filed_at": "2026-04-10"},
                      headers=_taxpayer("TIN-AUTHZ"))
    assert obj.status_code == 201, obj.text
    oid = obj.json()["objection_id"]
    dec = client.post(f"/v1/objections/{oid}/decision",
                      json={"outcome": "rejected", "decided_at": "2026-05-01"},
                      headers=OFFICER)
    assert dec.status_code == 200, dec.text
    # taxpayer cannot decide their own objection
    dec2 = client.post(f"/v1/objections/{oid}/decision",
                       json={"outcome": "upheld", "decided_at": "2026-05-01"},
                       headers=_taxpayer("TIN-AUTHZ"))
    assert dec2.status_code == 403


# ---- TIN ownership scoping (wrong tenant/taxpayer -> 403) ----

def test_taxpayer_cross_tin_file_denied():
    resp = client.post("/v1/filings/vat", json=_vat_body("TIN-OTHER"),
                       headers=_taxpayer("TIN-AUTHZ"))
    assert resp.status_code == 403


def test_taxpayer_cross_tin_read_denied():
    client.post("/v1/filings/vat", json=_vat_body("TIN-AUTHZ-2"),
                headers=OFFICER)
    resp = client.get("/v1/filings/vat/TIN-AUTHZ-2/2026-03",
                      headers=_taxpayer("TIN-UNRELATED"))
    assert resp.status_code == 403


def test_taxpayer_own_tin_ok():
    resp = client.post("/v1/filings/vat", json=_vat_body("TIN-MINE"),
                       headers=_taxpayer("TIN-MINE"))
    assert resp.status_code == 201, resp.text
    got = client.get("/v1/filings/vat/TIN-MINE/2026-03",
                     headers=_taxpayer("TIN-MINE"))
    assert got.status_code == 200


def test_taxpayer_cannot_object_on_others_assessment():
    r = client.post("/v1/assessments", json=_asm_body("TIN-AUTHZ-3"),
                    headers=OFFICER)
    aid = r.json()["assessment_id"]
    resp = client.post(f"/v1/assessments/{aid}/objections",
                       json={"grounds": "g", "admitted_amount_kobo": 0,
                             "filed_at": "2026-04-10"},
                       headers=_taxpayer("TIN-UNRELATED"))
    assert resp.status_code == 403


def test_auditor_reads_but_cannot_file():
    r = client.post("/v1/assessments", json=_asm_body("TIN-AUTHZ-4"),
                    headers=OFFICER)
    aid = r.json()["assessment_id"]
    assert client.get(f"/v1/assessments/{aid}", headers=AUDITOR).status_code == 200
    resp = client.post("/v1/filings/vat", json=_vat_body(), headers=AUDITOR)
    assert resp.status_code == 403
