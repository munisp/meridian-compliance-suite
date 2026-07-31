"""Prod-mode (keycloak RS256/JWKS) tests for meridian_py.dev_jwt, plus the
fail-closed contract when AUTH_MODE=keycloak is not configured."""
import os
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import jwt as pyjwt
import pytest
from cryptography.hazmat.primitives.asymmetric import rsa
from fastapi import Depends, FastAPI
from fastapi.testclient import TestClient

from meridian_py import dev_jwt
from meridian_py.dev_jwt import AuthDep, issue_token

ISSUER = "https://keycloak:8443/realms/meridian"
AUD = "meridian-services"

_priv = rsa.generate_private_key(public_exponent=65537, key_size=2048)
_other = rsa.generate_private_key(public_exponent=65537, key_size=2048)


def make_token(key=_priv, **over):
    claims = {
        "sub": "svc-account", "iss": ISSUER, "aud": AUD,
        "exp": int(time.time()) + 3600, "iat": int(time.time()),
        "realm_access": {"roles": ["operator"]},
        "resource_access": {AUD: {"roles": ["svc"]}},
    }
    claims.update(over)
    return pyjwt.encode(claims, key, algorithm="RS256", headers={"kid": "k1"})


class _FakeSigningKey:
    def __init__(self, key):
        self.key = key


class _FakeJWKClient:
    def __init__(self, url):
        self.uri = url

    def get_signing_key_from_jwt(self, token):
        return _FakeSigningKey(_priv.public_key())


@pytest.fixture()
def keycloak_mode(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "keycloak")
    monkeypatch.setenv("KEYCLOAK_ISSUER", ISSUER)
    monkeypatch.setenv("KEYCLOAK_AUDIENCE", AUD)
    monkeypatch.delenv("KEYCLOAK_JWKS_URL", raising=False)
    monkeypatch.setattr(dev_jwt, "_jwks_client", None)
    monkeypatch.setattr(pyjwt, "PyJWKClient", _FakeJWKClient)
    yield
    monkeypatch.setattr(dev_jwt, "_jwks_client", None)


def _app():
    app = FastAPI()

    @app.get("/v1/ping")
    def ping(principal=AuthDep):
        return {"sub": principal.sub, "roles": principal.roles}

    return app


def test_keycloak_valid_token(keycloak_mode):
    r = TestClient(_app()).get("/v1/ping",
                               headers={"Authorization": f"Bearer {make_token()}"})
    assert r.status_code == 200
    body = r.json()
    assert body["sub"] == "svc-account"
    assert set(body["roles"]) == {"operator", "svc"}


def test_keycloak_rejects_wrong_key(keycloak_mode):
    r = TestClient(_app()).get(
        "/v1/ping", headers={"Authorization": f"Bearer {make_token(key=_other)}"})
    assert r.status_code == 401


def test_keycloak_rejects_hs256_dev_token(keycloak_mode):
    # forged token: HS256 signed with the well-known dev secret
    forged = issue_token(sub="attacker", roles=["admin"])
    r = TestClient(_app()).get("/v1/ping",
                               headers={"Authorization": f"Bearer {forged}"})
    assert r.status_code == 401


def test_keycloak_rejects_bad_audience(keycloak_mode):
    r = TestClient(_app()).get(
        "/v1/ping", headers={"Authorization": f"Bearer {make_token(aud='other')}"})
    assert r.status_code == 401


def test_keycloak_rejects_bad_issuer(keycloak_mode):
    r = TestClient(_app()).get(
        "/v1/ping",
        headers={"Authorization": f"Bearer {make_token(iss='https://evil')}"})
    assert r.status_code == 401


def test_keycloak_rejects_expired(keycloak_mode):
    tok = make_token(exp=int(time.time()) - 60, iat=int(time.time()) - 120)
    r = TestClient(_app()).get("/v1/ping",
                               headers={"Authorization": f"Bearer {tok}"})
    assert r.status_code == 401


def test_keycloak_rejects_dev_role_header(keycloak_mode):
    r = TestClient(_app()).get("/v1/ping", headers={"X-Dev-Role": "admin"})
    assert r.status_code == 401


def test_fail_closed_when_issuer_unset(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "keycloak")
    monkeypatch.delenv("KEYCLOAK_ISSUER", raising=False)
    with pytest.raises(RuntimeError):
        dev_jwt.validate_auth_config()
    r = TestClient(_app(), raise_server_exceptions=False).get(
        "/v1/ping", headers={"X-Dev-Role": "admin"})
    assert r.status_code == 401


def test_dev_mode_still_works(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "dev")
    monkeypatch.delenv("KEYCLOAK_ISSUER", raising=False)
    dev_jwt.validate_auth_config()  # must not raise
    tok = issue_token(sub="dev-user", roles=["operator"])
    r = TestClient(_app()).get("/v1/ping",
                               headers={"Authorization": f"Bearer {tok}"})
    assert r.status_code == 200
    r = TestClient(_app()).get("/v1/ping", headers={"X-Dev-Role": "auditor"})
    assert r.status_code == 200
    r = TestClient(_app()).get("/v1/ping", headers={"X-Dev-Role": "root"})
    assert r.status_code == 401


def test_require_roles(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "dev")
    app = FastAPI()

    @app.get("/v1/admin")
    def admin(principal=Depends(dev_jwt.require_roles("admin"))):
        return {"ok": True}

    c = TestClient(app)
    assert c.get("/v1/admin", headers={"X-Dev-Role": "admin"}).status_code == 200
    assert c.get("/v1/admin", headers={"X-Dev-Role": "auditor"}).status_code == 403
    assert c.get("/v1/admin").status_code == 401
