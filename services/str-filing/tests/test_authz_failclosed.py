"""A1-03 regression: keycloak mode must never silently downgrade token
verification to the HS256 dev secret when meridian_py is unavailable.

Pre-fix: with AUTH_MODE=keycloak and meridian_py unimportable, a token
signed with the public dev secret authenticated as admin (auth bypass).
Post-fix: boot validation raises and _principal denies.
"""
import sys

import jwt
import pytest
from fastapi import Request

from app import authz

DEV_SECRET = "meridian-dev-secret-change-me-32!"


def _bearer_request(token: str) -> Request:
    return Request({"type": "http", "method": "POST", "path": "/",
                    "headers": [(b"authorization", f"Bearer {token}".encode())]})


def _hide_meridian_py(monkeypatch):
    monkeypatch.setitem(sys.modules, "meridian_py", None)
    monkeypatch.setitem(sys.modules, "meridian_py.dev_jwt", None)


def test_keycloak_missing_meridian_py_boot_fails_closed(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "keycloak")
    monkeypatch.setenv("PROFILE", "prod")
    monkeypatch.setenv("PERMIFY_URL", "http://permify:3476")  # isolate auth check
    _hide_meridian_py(monkeypatch)
    with pytest.raises(RuntimeError):
        authz.validate_authz_config()


def test_keycloak_no_hs256_dev_secret_downgrade(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "keycloak")
    monkeypatch.delenv("MERIDIAN_DEV_JWT_SECRET", raising=False)
    _hide_meridian_py(monkeypatch)
    forged = jwt.encode({"sub": "attacker", "roles": ["admin"]},
                        DEV_SECRET, algorithm="HS256")
    principal = authz._principal(_bearer_request(forged))
    assert not principal.get("sub"), (
        "keycloak mode accepted an HS256 dev-secret token (auth bypass)")


def test_dev_hs256_still_works(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "dev")
    monkeypatch.setenv("PROFILE", "dev")  # B2 #17: dev fallback is dev-only
    _hide_meridian_py(monkeypatch)
    tok = jwt.encode({"sub": "dev-user", "roles": ["admin"]},
                     DEV_SECRET, algorithm="HS256")
    principal = authz._principal(_bearer_request(tok))
    assert principal.get("sub") == "dev-user"
