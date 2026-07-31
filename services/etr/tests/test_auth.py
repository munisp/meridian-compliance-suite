"""Fail-closed auth tests for etr (PB-6): keycloak mode without issuer must
refuse to start and deny requests; /v1/dev-token and X-Dev-Role are dev-only
with an allowlist."""
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
os.environ["ETR_DB"] = ":memory:"

import pytest
from fastapi.testclient import TestClient

from app.main import app


def test_dev_token_gated_to_dev_mode(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "keycloak")
    monkeypatch.setenv("KEYCLOAK_ISSUER", "https://keycloak:8443/realms/meridian")
    c = TestClient(app)
    r = c.post("/v1/dev-token", json={"sub": "x", "roles": ["admin"]})
    assert r.status_code == 404


def test_dev_token_works_in_dev_mode(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "dev")
    monkeypatch.delenv("KEYCLOAK_ISSUER", raising=False)
    c = TestClient(app)
    r = c.post("/v1/dev-token", json={"sub": "x", "roles": ["operator"]})
    assert r.status_code == 200
    assert r.json()["token"]


def test_keycloak_without_issuer_fails_closed_at_startup(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "keycloak")
    monkeypatch.delenv("KEYCLOAK_ISSUER", raising=False)
    with pytest.raises(RuntimeError):
        with TestClient(app):
            pass


def test_keycloak_without_issuer_denies_requests(monkeypatch):
    # even if somehow running, requests must be denied (defense in depth)
    monkeypatch.setenv("AUTH_MODE", "keycloak")
    monkeypatch.delenv("KEYCLOAK_ISSUER", raising=False)
    c = TestClient(app)
    r = c.get("/v1/packs", headers={"X-Dev-Role": "admin"})
    assert r.status_code == 401


def test_dev_role_allowlist(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "dev")
    monkeypatch.delenv("KEYCLOAK_ISSUER", raising=False)
    c = TestClient(app)
    assert c.get("/v1/packs", headers={"X-Dev-Role": "operator"}).status_code == 200
    assert c.get("/v1/packs", headers={"X-Dev-Role": "root"}).status_code == 401
    assert c.get("/v1/packs", headers={"X-Dev-Role": "superuser"}).status_code == 401
