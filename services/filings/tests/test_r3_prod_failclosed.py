"""R3 verifier regression (filings #41): PROFILE=prod with AUTH_MODE unset
previously resolved to the shared lib's dev default, so a forgeable
`X-Dev-Role: operator` header issued a statutory assessment (201).

Post-fix contract (ported into shared meridian_py.dev_jwt so every consumer
gets it — same fail-closed gate as str-filing):
- boot: validate_auth_config() raises under PROFILE=prod unless AUTH_MODE is
  keycloak/prod AND OIDC issuer config is present (boot refusal)
- request: even if somehow running, require_auth denies dev credentials
  (X-Dev-Role / dev-HS256 Bearer) with 401 under PROFILE=prod
- PROFILE=dev keeps the dev path working (no regression for developers)
"""
import jwt as pyjwt
import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from meridian_py import dev_jwt


def _app() -> FastAPI:
    app = FastAPI()

    @app.post("/v1/assessments")
    async def _asm(p=dev_jwt.AuthDep):  # noqa: B008
        return {"ok": True, "sub": p.sub}

    return app


@pytest.fixture
def prod_unset(monkeypatch):
    monkeypatch.setenv("PROFILE", "prod")
    monkeypatch.delenv("AUTH_MODE", raising=False)
    monkeypatch.delenv("KEYCLOAK_ISSUER", raising=False)
    monkeypatch.delenv("KEYCLOAK_JWKS_URL", raising=False)


def test_prod_unset_authmode_refuses_boot(prod_unset):
    with pytest.raises(RuntimeError):
        dev_jwt.validate_auth_config()


def test_prod_unset_authmode_forged_dev_role_401(prod_unset):
    client = TestClient(_app())
    r = client.post("/v1/assessments", headers={"X-Dev-Role": "operator"})
    assert r.status_code == 401, r.text


def test_prod_unset_authmode_dev_hs256_token_401(prod_unset):
    tok = pyjwt.encode({"sub": "attacker", "roles": ["operator"]},
                       dev_jwt._secret(), algorithm="HS256")
    client = TestClient(_app())
    r = client.post("/v1/assessments",
                    headers={"Authorization": f"Bearer {tok}"})
    assert r.status_code == 401, r.text


def test_dev_profile_dev_path_still_works(monkeypatch):
    monkeypatch.setenv("PROFILE", "dev")
    monkeypatch.delenv("AUTH_MODE", raising=False)
    dev_jwt.validate_auth_config()  # must not raise
    client = TestClient(_app())
    r = client.post("/v1/assessments", headers={"X-Dev-Role": "operator"})
    assert r.status_code == 200, r.text
