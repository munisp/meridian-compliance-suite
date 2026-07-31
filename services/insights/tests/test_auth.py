"""Auth tests for insights (PB-5): endpoints require authentication;
write endpoints require an admin/operator role; keycloak mode rejects
HS256 dev tokens (forged with the public dev secret)."""
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


def make_rs256_token(**over):
    claims = {"sub": "svc", "iss": ISSUER, "aud": AUD,
              "exp": int(time.time()) + 3600, "iat": int(time.time()),
              "realm_access": {"roles": ["operator"]}}
    claims.update(over)
    return pyjwt.encode(claims, _priv, algorithm="RS256", headers={"kid": "k1"})


class _FakeSigningKey:
    key = _priv.public_key()


class _FakeJWKClient:
    def __init__(self, url):
        self.uri = url

    def get_signing_key_from_jwt(self, token):
        return _FakeSigningKey()


@pytest.fixture()
def client():
    os.environ.pop("KEYCLOAK_ISSUER", None)
    os.environ["AUTH_MODE"] = "dev"
    from app.main import app
    return TestClient(app)


def test_unauthenticated_rejected(client):
    assert client.post("/v1/insights/circularity", json={"invoices": []}).status_code == 401
    assert client.post("/v1/insights/penalties", json={}).status_code == 401
    assert client.post("/v1/insights/explain", json={}).status_code == 401
    assert client.get("/v1/insights/fx/audit").status_code == 401


def test_auditor_cannot_write(client):
    r = client.post("/v1/insights/circularity", json={"invoices": []},
                    headers={"X-Dev-Role": "auditor"})
    assert r.status_code == 403
    # but auditor can read
    assert client.get("/v1/insights/fx/audit",
                      headers={"X-Dev-Role": "auditor"}).status_code == 200


def test_operator_can_write(client):
    r = client.post("/v1/insights/circularity", json={"invoices": []},
                    headers={"X-Dev-Role": "operator"})
    assert r.status_code == 200


def test_keycloak_mode_rejects_forged_hs256(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "keycloak")
    monkeypatch.setenv("KEYCLOAK_ISSUER", ISSUER)
    monkeypatch.setenv("KEYCLOAK_AUDIENCE", AUD)
    monkeypatch.setattr(dev_jwt, "_jwks_client", None)
    monkeypatch.setattr(pyjwt, "PyJWKClient", _FakeJWKClient)
    from app.main import app
    c = TestClient(app)
    forged = issue_token(sub="attacker", roles=["admin"])
    r = c.post("/v1/insights/circularity", json={"invoices": []},
               headers={"Authorization": f"Bearer {forged}"})
    assert r.status_code == 401
    r = c.post("/v1/insights/circularity", json={"invoices": []},
               headers={"Authorization": f"Bearer {make_rs256_token()}"})
    assert r.status_code == 200
