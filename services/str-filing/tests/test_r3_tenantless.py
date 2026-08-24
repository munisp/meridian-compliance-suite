"""R3 verifier regression (str-filing #39): a verified principal with an
EMPTY tenant claim was a cross-tenant wildcard — `if ptenant and tenant_id
and tenant_id != ptenant` failed open, so a tenantless JWT could read any
other tenant's STR (200) and file an STR into another tenant (201).

Post-fix (mirrors einvoicing tenantGuard semantics from #38): tenant-scoped
STR routes require a non-empty tenant claim; empty claim -> 403 on read,
create and list; the created record is bound to the caller's tenant only.
"""
import jwt
import pytest
from fastapi.testclient import TestClient

from app import db
from app.main import app, sessions

client = TestClient(app)


@pytest.fixture(autouse=True)
def _cleanup():
    yield
    # do not leak pending STRs into other test modules (shared test DB)
    with sessions() as s:
        s.query(db.STRFiling).filter(
            db.STRFiling.tenant_id.in_([TENANT, OTHER])).delete()
        s.commit()

TENANT = "t-bank-r3"
OTHER = "t-bank-r3-other"
SECRET = "meridian-dev-secret-change-me-32!"


def _tok(tenant: str) -> str:
    claims = {"sub": "officer-r3", "roles": ["compliance-officer"]}
    if tenant:
        claims["tenant_id"] = tenant
    return jwt.encode(claims, SECRET, algorithm="HS256")


TENANTLESS = {"Authorization": f"Bearer {_tok('')}"}
OFFICER = {"Authorization": f"Bearer {_tok(TENANT)}"}


def _payload(key: str, tenant: str) -> dict:
    return {"tenant_id": tenant, "idempotency_key": key,
            "subject_ref": "cust-r3", "report_type": "STR",
            "payload": {"amount": 1, "currency": "NGN", "trigger": "t"}}


def test_empty_tenant_claim_cannot_read_other_tenant():
    rec = client.post("/v1/str", json=_payload("r3-read-1", OTHER),
                      headers={"Authorization": f"Bearer {_tok(OTHER)}"})
    assert rec.status_code == 201, rec.text
    str_id = rec.json()["id"]
    # tenantless principal tries to read the other tenant's STR
    resp = client.get(f"/v1/str/{str_id}", headers=TENANTLESS)
    assert resp.status_code == 403, resp.text


def test_empty_tenant_claim_cannot_create_cross_tenant():
    resp = client.post("/v1/str", json=_payload("r3-create-1", OTHER),
                       headers=TENANTLESS)
    assert resp.status_code == 403, resp.text
    # and nothing was filed under the attacker key
    assert client.get("/v1/str", headers=OFFICER).status_code == 200
    resp2 = client.post("/v1/str", json=_payload("r3-create-2", TENANT),
                        headers=TENANTLESS)
    assert resp2.status_code == 403, resp2.text


def test_empty_tenant_claim_list_denied():
    resp = client.get("/v1/str", headers=TENANTLESS)
    assert resp.status_code == 403, resp.text


def test_tenant_claim_still_scopes_normally():
    rec = client.post("/v1/str", json=_payload("r3-ok-1", TENANT),
                      headers=OFFICER)
    assert rec.status_code == 201, rec.text
    assert rec.json()["tenant_id"] == TENANT
    assert client.get(f"/v1/str/{rec.json()['id']}",
                      headers=OFFICER).status_code == 200
