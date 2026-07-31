"""Auth tests for rev360 (PB-3/PB-4): /v1/sso/login is dev-only; keycloak
mode rejects HS256 dev tokens forged with the public dev secret."""
import os
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import jwt as pyjwt
import pytest
from cryptography.hazmat.primitives.asymmetric import rsa
from fastapi.testclient import TestClient

from meridian_py import dev_jwt
from meridian_py.dev_jwt import issue_token

ISSUER = "https://keycloak:8443/realms/meridian"
AUD = "meridian-services"
_priv = rsa.generate_private_key(public_exponent=65537, key_size=2048)


class _FakeSigningKey:
    key = _priv.public_key()


class _FakeJWKClient:
    def __init__(self, url):
        self.uri = url

    def get_signing_key_from_jwt(self, token):
        return _FakeSigningKey()


def test_sso_login_dev_mode_issues_token():
    os.environ["AUTH_MODE"] = "dev"
    os.environ.pop("KEYCLOAK_ISSUER", None)
    from app.main import app
    c = TestClient(app)
    r = c.get("/v1/sso/login", params={"consultant": "a@b.c", "role": "operator"})
    assert r.status_code == 200
    assert r.json()["token"]


def test_sso_login_disabled_in_keycloak_mode(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "keycloak")
    monkeypatch.setenv("KEYCLOAK_ISSUER", ISSUER)
    from app.main import app
    c = TestClient(app)
    r = c.get("/v1/sso/login", params={"role": "admin", "tenant_id": "t1"})
    assert r.status_code == 404


def test_keycloak_mode_rejects_forged_hs256(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "keycloak")
    monkeypatch.setenv("KEYCLOAK_ISSUER", ISSUER)
    monkeypatch.setenv("KEYCLOAK_AUDIENCE", AUD)
    monkeypatch.setattr(dev_jwt, "_jwks_client", None)
    monkeypatch.setattr(pyjwt, "PyJWKClient", _FakeJWKClient)
    from app.main import app
    c = TestClient(app)
    forged = issue_token(sub="attacker", roles=["admin"], tenant_id="any")
    r = c.get("/v1/records", headers={"Authorization": f"Bearer {forged}"})
    assert r.status_code == 401
    # X-Dev-Role is not honored in keycloak mode either
    assert c.get("/v1/records", headers={"X-Dev-Role": "admin"}).status_code == 401


def test_keycloak_mode_accepts_rs256(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "keycloak")
    monkeypatch.setenv("KEYCLOAK_ISSUER", ISSUER)
    monkeypatch.setenv("KEYCLOAK_AUDIENCE", AUD)
    monkeypatch.setattr(dev_jwt, "_jwks_client", None)
    monkeypatch.setattr(pyjwt, "PyJWKClient", _FakeJWKClient)
    from app.main import app
    c = TestClient(app)
    claims = {"sub": "svc", "iss": ISSUER, "aud": AUD,
              "exp": int(time.time()) + 3600, "iat": int(time.time()),
              "realm_access": {"roles": ["operator"]}}
    tok = pyjwt.encode(claims, _priv, algorithm="RS256", headers={"kid": "k1"})
    assert c.get("/v1/records", headers={"Authorization": f"Bearer {tok}"}).status_code == 200
