"""Dev JWT auth (SPEC 1.3): HS256 Bearer tokens, plus X-Dev-Role header when
AUTH_MODE=dev. Prod mode would validate Keycloak OIDC JWKS (OIDC_ISSUER_URL).
"""

from __future__ import annotations

import os
import time

import jwt
from fastapi import Depends, Request
from fastapi.responses import JSONResponse


def _secret() -> str:
    return os.environ.get("MERIDIAN_DEV_JWT_SECRET",
                          "meridian-dev-secret-change-me-32!")


def issue_token(sub: str, roles: list[str] | None = None,
                tenant_id: str = "", ttl_seconds: int = 8 * 3600) -> str:
    now = int(time.time())
    return jwt.encode(
        {"sub": sub, "roles": roles or ["operator"], "tenant_id": tenant_id,
         "iat": now, "exp": now + ttl_seconds},
        _secret(), algorithm="HS256",
    )


def verify_token(token: str) -> dict:
    return jwt.decode(token, _secret(), algorithms=["HS256"])


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
    """FastAPI dependency: Bearer JWT, or X-Dev-Role when AUTH_MODE=dev."""
    from fastapi import HTTPException

    auth = request.headers.get("authorization", "")
    if auth.startswith("Bearer "):
        try:
            return Principal(verify_token(auth[len("Bearer "):]))
        except jwt.PyJWTError as exc:
            raise HTTPException(status_code=401, detail=str(exc)) from exc
    if os.environ.get("AUTH_MODE", "dev") == "dev":
        role = request.headers.get("x-dev-role", "")
        if role in ("admin", "operator", "auditor"):
            return Principal({"sub": f"dev-{role}", "roles": [role],
                              "tenant_id": request.headers.get("x-tenant-id", "")})
    raise HTTPException(status_code=401,
                        detail="provide Bearer JWT or X-Dev-Role (dev mode)")


# Convenience alias for routers
AuthDep = Depends(require_auth)
