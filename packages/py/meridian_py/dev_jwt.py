"""Meridian auth (SPEC 1.3): fail-closed Bearer JWT for FastAPI services.

Modes (selected by AUTH_MODE):
  - dev (default): HS256 Bearer tokens (MERIDIAN_DEV_JWT_SECRET) plus the
    X-Dev-Role header allowlist (admin|operator|auditor).
  - keycloak (alias: prod): RS256 tokens verified against the realm JWKS
    (KEYCLOAK_ISSUER / KEYCLOAK_AUDIENCE / KEYCLOAK_JWKS_URL) via PyJWKClient
    (key cache + refresh-on-unknown-kid); enforces iss/aud/exp and maps
    realm_access.roles + resource_access[audience].roles into `roles`.

FAIL-CLOSED CONTRACT: AUTH_MODE=keycloak without KEYCLOAK_ISSUER refuses to
start (validate_auth_config, called at service import) and denies every
request at the dependency level. There is no silent fallback to dev auth.
"""

from __future__ import annotations

import os
import time

import jwt
from fastapi import Depends, Request
from fastapi.responses import JSONResponse

_DEV_ROLES = ("admin", "operator", "auditor")


def _secret() -> str:
    return os.environ.get("MERIDIAN_DEV_JWT_SECRET",
                          "meridian-dev-secret-change-me-32!")


def _mode() -> str:
    m = os.environ.get("AUTH_MODE", "dev").lower()
    return "keycloak" if m in ("keycloak", "prod") else m


def _keycloak_configured() -> bool:
    return bool(os.environ.get("KEYCLOAK_ISSUER")
                or os.environ.get("KEYCLOAK_JWKS_URL"))


def _profile_prod() -> bool:
    """PROFILE=prod means production regardless of AUTH_MODE (R3 verifier
    hole in filings #41: PROFILE=prod + AUTH_MODE unset silently resolved
    to dev auth and honoured the forgeable X-Dev-Role header)."""
    return os.environ.get("PROFILE", "").strip().lower() == "prod"


def validate_auth_config() -> None:
    """Fail closed at startup: a keycloak/prod deployment missing its OIDC
    issuer configuration must refuse to boot rather than run dev auth."""
    if _profile_prod() and _mode() != "keycloak":
        raise RuntimeError(
            "PROFILE=prod but AUTH_MODE is unset/not keycloak; refusing to "
            "start (no dev auth in prod)")
    if _mode() == "keycloak" and not _keycloak_configured():
        raise RuntimeError(
            "AUTH_MODE=keycloak but KEYCLOAK_ISSUER/KEYCLOAK_JWKS_URL is "
            "unset; refusing to start (no dev fallback)")


def issue_token(sub: str, roles: list[str] | None = None,
                tenant_id: str = "", ttl_seconds: int = 8 * 3600) -> str:
    now = int(time.time())
    return jwt.encode(
        {"sub": sub, "roles": roles or ["operator"], "tenant_id": tenant_id,
         "iat": now, "exp": now + ttl_seconds},
        _secret(), algorithm="HS256",
    )


def verify_token(token: str) -> dict:
    """Dev-mode HS256 verification (tests/tooling)."""
    return jwt.decode(token, _secret(), algorithms=["HS256"])


# ---------------- Keycloak RS256 / JWKS ----------------

_jwks_client = None


def _jwks():
    global _jwks_client
    issuer = os.environ.get("KEYCLOAK_ISSUER", "").rstrip("/")
    jwks_url = (os.environ.get("KEYCLOAK_JWKS_URL")
                or f"{issuer}/protocol/openid-connect/certs")
    # Rebuild the client when the configured URL changes (env swap in tests).
    if _jwks_client is None or _jwks_client.uri != jwks_url:
        _jwks_client = jwt.PyJWKClient(jwks_url)
    return _jwks_client


def verify_keycloak_token(token: str) -> dict:
    """Verify an RS256 Keycloak token against the realm JWKS; validates
    signature, iss, exp and aud (when KEYCLOAK_AUDIENCE is set) and maps
    realm + client roles into a flat `roles` claim."""
    issuer = os.environ.get("KEYCLOAK_ISSUER", "").rstrip("/")
    audience = os.environ.get("KEYCLOAK_AUDIENCE", "")
    try:
        key = _jwks().get_signing_key_from_jwt(token).key
        payload = jwt.decode(
            token, key, algorithms=["RS256"],
            issuer=issuer or None,
            audience=audience or None,
            options={"verify_aud": bool(audience),
                     "verify_iss": bool(issuer),
                     "require": ["exp", "sub"]})
    except jwt.PyJWTError as exc:
        raise ValueError(str(exc)) from exc
    roles = list(payload.get("realm_access", {}).get("roles", []))
    if audience:
        roles += payload.get("resource_access", {}).get(audience, {}).get("roles", [])
    payload["roles"] = roles
    return payload


def problem(status: int, title: str, detail: str = "") -> JSONResponse:
    """RFC7807 problem+json response."""
    return JSONResponse(
        status_code=status,
        media_type="application/problem+json",
        content={"type": "about:blank", "title": title,
                 "status": status, "detail": detail},
    )


class Principal(dict):
    @property
    def sub(self) -> str:
        return self.get("sub", "")

    @property
    def roles(self) -> list[str]:
        return self.get("roles", [])

    @property
    def tenant_id(self) -> str:
        return self.get("tenant_id", "")


async def require_auth(request: Request) -> Principal:
    """FastAPI dependency: Bearer JWT (RS256 in keycloak mode, HS256 in dev),
    or X-Dev-Role when AUTH_MODE=dev. Fails closed when keycloak mode is
    selected but not configured."""
    from fastapi import HTTPException

    mode = _mode()
    auth = request.headers.get("authorization", "")
    if _profile_prod() and mode != "keycloak":
        # fail closed: prod profile with dev-resolved auth mode denies
        # every request (no X-Dev-Role / dev-HS256 path in prod)
        raise HTTPException(
            status_code=401,
            detail="PROFILE=prod requires AUTH_MODE=keycloak; dev auth refused")
    if mode == "keycloak":
        if not _keycloak_configured():
            # fail closed: misconfigured prod denies every request
            raise HTTPException(
                status_code=401,
                detail="AUTH_MODE=keycloak but KEYCLOAK_ISSUER/KEYCLOAK_JWKS_URL is unset")
        if auth.startswith("Bearer "):
            try:
                return Principal(verify_keycloak_token(auth[len("Bearer "):]))
            except ValueError as exc:
                raise HTTPException(status_code=401, detail=str(exc)) from exc
        raise HTTPException(status_code=401,
                            detail="Bearer JWT required (AUTH_MODE=keycloak)")
    if auth.startswith("Bearer "):
        try:
            return Principal(verify_token(auth[len("Bearer "):]))
        except jwt.PyJWTError as exc:
            raise HTTPException(status_code=401, detail=str(exc)) from exc
    if mode == "dev":
        role = request.headers.get("x-dev-role", "")
        if role in _DEV_ROLES:
            return Principal({"sub": f"dev-{role}", "roles": [role],
                              "tenant_id": request.headers.get("x-tenant-id", "")})
    raise HTTPException(status_code=401,
                        detail="provide Bearer JWT or X-Dev-Role (dev mode)")


def require_roles(*roles: str):
    """FastAPI dependency factory: authentication plus a role check (403)."""
    from fastapi import HTTPException

    async def _dep(request: Request) -> Principal:
        principal = await require_auth(request)
        if not set(roles) & set(principal.roles):
            raise HTTPException(
                status_code=403,
                detail=f"requires one of roles: {', '.join(roles)}")
        return principal

    return _dep


# Convenience alias for routers
AuthDep = Depends(require_auth)
