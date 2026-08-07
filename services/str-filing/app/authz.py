"""Authorization for privileged STR operations (DLQ requeue).

Python port of the case-mgmt Permify pattern (services/case-mgmt/permify.go):

- PERMIFY_URL set   -> live Permify Check API
  (POST /v1/tenants/{tenant}/permissions/check, entity/permission/subject
  refs in `type:id` form, one retry on 5xx, fail closed on error).
- PERMIFY_URL unset -> dev role fallback: the request principal must hold
  one of REQUEUE_DEV_ROLES (mirrors the dev file-backed checker in
  case-mgmt). PROFILE=prod / AUTH_MODE=keycloak + PERMIFY_URL unset fails
  closed at startup (no silent decentralized authz in prod).
"""
from __future__ import annotations

import logging
import os

import httpx
from fastapi import Request
from fastapi.responses import JSONResponse

log = logging.getLogger("str-filing.authz")

REQUEUE_PERMISSION = "requeue"
REQUEUE_DEV_ROLES = ("admin", "compliance-officer")


def _prod() -> bool:
    return (os.environ.get("PROFILE", "").lower() == "prod"
            or os.environ.get("AUTH_MODE", "").lower() in ("keycloak", "prod"))


def validate_authz_config() -> None:
    if _prod() and not os.environ.get("PERMIFY_URL"):
        raise RuntimeError(
            "prod profile but PERMIFY_URL is unset; refusing to start "
            "(no dev authz fallback)")


def _split_ref(ref: str) -> dict:
    typ, _, rid = ref.partition(":")
    if not typ or not rid:
        raise ValueError(f"permify reference {ref!r} must be type:id")
    return {"type": typ, "id": rid}


class PermifyClient:
    """Thin client for the Permify Check API v1 (mirrors case-mgmt)."""

    def __init__(self, base_url: str, tenant: str = "t1",
                 timeout: float = 2.0):
        self.base = base_url.rstrip("/")
        self.tenant = tenant or "t1"
        self._timeout = timeout

    def check(self, entity: str, permission: str, subject: str) -> bool:
        body = {"entity": _split_ref(entity), "permission": permission,
                "subject": _split_ref(subject)}
        url = f"{self.base}/v1/tenants/{self.tenant}/permissions/check"
        last_exc: Exception | None = None
        for attempt in range(2):  # one retry on 5xx/transport, like case-mgmt
            try:
                resp = httpx.post(url, json=body, timeout=self._timeout)
                if resp.status_code >= 500:
                    raise RuntimeError(f"permify check status {resp.status_code}")
                if resp.status_code != 200:
                    return False
                out = resp.json()
                if "allowed" in out:
                    return bool(out["allowed"])
                return out.get("can") == "RESULT_ALLOWED"
            except (httpx.HTTPError, RuntimeError) as exc:
                last_exc = exc
                log.warning("component=permify circuit: check %s#%s@%s "
                            "attempt %d failed: %s", entity, permission,
                            subject, attempt + 1, exc)
        raise PermissionError(f"permify unavailable: {last_exc}")


def permify_from_env() -> PermifyClient | None:
    base = os.environ.get("PERMIFY_URL")
    if not base:
        return None
    return PermifyClient(base, os.environ.get("PERMIFY_TENANT", "t1"))


def _principal(request: Request) -> dict:
    """Extract sub/roles/tenant from the Bearer JWT (HS256 dev secret or,
    when meridian_py is importable and AUTH_MODE=keycloak, the platform
    verifier) or the X-Dev-Role header allowlist in dev."""
    auth = request.headers.get("Authorization", "")
    token = auth[7:] if auth.startswith("Bearer ") else ""
    if token:
        try:
            from meridian_py import dev_jwt  # platform shared auth package
            if os.environ.get("AUTH_MODE", "dev").lower() in ("keycloak", "prod"):
                return dev_jwt.verify_keycloak_token(token)
            return dev_jwt.verify_token(token)
        except ImportError:
            import jwt
            secret = os.environ.get("MERIDIAN_DEV_JWT_SECRET",
                                    "meridian-dev-secret-change-me-32!")
            try:
                return jwt.decode(token, secret, algorithms=["HS256"])
            except jwt.PyJWTError:
                return {}
        except Exception:
            return {}
    role = request.headers.get("X-Dev-Role", "")
    if role and not _prod():
        return {"sub": f"dev-{role}", "roles": [role],
                "tenant_id": request.headers.get("X-Tenant-Id", "")}
    return {}


def problem(status: int, title: str, detail: str = "") -> JSONResponse:
    return JSONResponse(status_code=status,
                        media_type="application/problem+json",
                        content={"type": "about:blank", "title": title,
                                 "status": status, "detail": detail})


def authorize_requeue(request: Request, str_id: str,
                      permify: PermifyClient | None) -> dict | JSONResponse:
    """Gate dlq->pending requeue. Returns the principal dict, or an
    RFC7807 problem response when denied."""
    principal = _principal(request)
    if not principal.get("sub"):
        return problem(401, "unauthorized", "authentication required")
    if permify is not None:
        subject = f"user:{principal['sub']}"
        try:
            allowed = permify.check(f"str_filing:{str_id}",
                                    REQUEUE_PERMISSION, subject)
        except PermissionError as exc:  # fail closed
            return problem(503, "authorization unavailable", str(exc))
        if not allowed:
            return problem(403, "forbidden",
                           f"str_filing#{REQUEUE_PERMISSION} denied for "
                           f"{subject}")
        return principal
    if not set(REQUEUE_DEV_ROLES) & set(principal.get("roles", [])):
        return problem(403, "forbidden",
                       f"requires one of {sorted(REQUEUE_DEV_ROLES)}")
    return principal
