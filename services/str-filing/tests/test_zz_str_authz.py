"""B2 regression (findings #2/#17): STR create/read/list require
authentication; actor is bound to the JWT principal (never X-Actor header
or request body); list is scoped to the caller's tenant; the X-Dev-Role /
HS256 dev-secret fallback is honoured only when PROFILE=dev.

Pre-fix: anonymous create/read/list succeeded; actor came from the
client-controlled X-Actor header; list was cross-tenant via the optional
tenant_id filter; X-Dev-Role was honoured whenever PROFILE != prod.
"""
import jwt
import pytest
from fastapi import Request
from fastapi.testclient import TestClient

from app import authz
from app.main import app

client = TestClient(app)

TENANT = "t-bank-authz"
OTHER = "t-bank-other"

OFFICER = {"X-Dev-Role": "compliance-officer", "X-Tenant-Id": TENANT}
OFFICER_OTHER = {"X-Dev-Role": "compliance-officer", "X-Tenant-Id": OTHER}


def _payload(key: str, tenant: str = TENANT, actor: str = "") -> dict:
    body = {"tenant_id": tenant, "idempotency_key": key,
            "subject_ref": "cust-authz", "report_type": "STR",
            "payload": {"amount": 1, "currency": "NGN", "trigger": "t"}}
    if actor:
        body["actor"] = actor
    return body


# ---- anonymous access is denied (pre-fix: all succeeded) ----

def test_anonymous_create_denied():
    resp = client.post("/v1/str", json=_payload("anon-1"))
    assert resp.status_code == 401


def test_anonymous_get_denied():
    rec = client.post("/v1/str", json=_payload("anon-2"),
                      headers=OFFICER).json()
    assert client.get(f"/v1/str/{rec['id']}").status_code == 401


def test_anonymous_list_denied():
    assert client.get("/v1/str").status_code == 401


# ---- actor binding: JWT principal wins over X-Actor header / body ----

def test_actor_from_principal_not_header_or_body():
    resp = client.post(
        "/v1/str", json=_payload("actor-1", actor="spoofed-body-actor"),
        headers={**OFFICER, "X-Actor": "spoofed-header-actor"})
    assert resp.status_code == 201, resp.text
    rec = resp.json()
    assert rec["created_by"] == "dev-compliance-officer"
    assert "spoofed" not in rec["created_by"]


def test_read_requires_role():
    rec = client.post("/v1/str", json=_payload("role-1"),
                      headers=OFFICER).json()
    resp = client.get(f"/v1/str/{rec['id']}",
                      headers={"X-Dev-Role": "taxpayer",
                               "X-Tenant-Id": TENANT})
    assert resp.status_code == 403


# ---- tenant scoping ----

def test_create_tenant_mismatch_denied():
    resp = client.post("/v1/str", json=_payload("ten-x", tenant=OTHER),
                       headers=OFFICER)
    assert resp.status_code == 403


def test_get_cross_tenant_denied():
    rec = client.post("/v1/str", json=_payload("ten-get"),
                      headers=OFFICER).json()
    resp = client.get(f"/v1/str/{rec['id']}", headers=OFFICER_OTHER)
    assert resp.status_code == 403


def test_list_scoped_to_caller_tenant():
    client.post("/v1/str", json=_payload("scope-a"), headers=OFFICER)
    client.post("/v1/str", json=_payload("scope-b", tenant=OTHER),
                headers=OFFICER_OTHER)
    mine = client.get("/v1/str", headers=OFFICER).json()
    assert mine and all(r["tenant_id"] == TENANT for r in mine)
    theirs = client.get("/v1/str", headers=OFFICER_OTHER).json()
    assert theirs and all(r["tenant_id"] == OTHER for r in theirs)
    # explicit cross-tenant filter is refused
    resp = client.get("/v1/str", params={"tenant_id": OTHER},
                      headers=OFFICER)
    assert resp.status_code == 403


# ---- #17: dev fallback honoured only when PROFILE=dev ----

def _devrole_request(role: str) -> Request:
    return Request({"type": "http", "method": "GET", "path": "/",
                    "headers": [(b"x-dev-role", role.encode())]})


def test_dev_role_ignored_when_profile_not_dev(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "dev")
    monkeypatch.delenv("PROFILE", raising=False)  # not dev -> fail closed
    principal = authz._principal(_devrole_request("admin"))
    assert not principal.get("sub"), (
        "X-Dev-Role honoured with PROFILE unset (must be PROFILE=dev only)")


def test_hs256_default_secret_ignored_when_profile_not_dev(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "dev")
    monkeypatch.delenv("PROFILE", raising=False)
    monkeypatch.delenv("MERIDIAN_DEV_JWT_SECRET", raising=False)
    monkeypatch.setitem(__import__("sys").modules, "meridian_py", None)
    monkeypatch.setitem(__import__("sys").modules, "meridian_py.dev_jwt", None)
    forged = jwt.encode({"sub": "attacker", "roles": ["admin"]},
                        "meridian-dev-secret-change-me-32!",
                        algorithm="HS256")
    req = Request({"type": "http", "method": "GET", "path": "/",
                   "headers": [(b"authorization",
                                f"Bearer {forged}".encode())]})
    assert not authz._principal(req).get("sub"), (
        "HS256 dev default secret accepted outside PROFILE=dev")


def test_dev_role_ok_when_profile_dev(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "dev")
    monkeypatch.setenv("PROFILE", "dev")
    principal = authz._principal(_devrole_request("admin"))
    assert principal.get("sub") == "dev-admin"
