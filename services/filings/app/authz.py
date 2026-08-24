"""Authentication & authorization for the filings service (B2 finding #1).

Pre-fix the service had NO auth on any route: anonymous callers could file
VAT/PAYE/CIT returns, issue statutory assessments and decide objections for
any TIN.

Model (mirrors the str-filing post-fix pattern, built on the platform
meridian_py.dev_jwt fail-closed verifier):

- every route requires a verified principal (401 when absent)
- filing routes (VAT/PAYE/CIT compute/reads) require one of
  FILE_ROLES; taxpayer principals are additionally scoped to their own
  TIN(s) (403 on cross-TIN access)
- assessment issue, objection decision, tick and TAT referrals are
  officer-only (OFFICER_ROLES = operator|admin)
- auditors are read-only
"""
from __future__ import annotations

from fastapi import HTTPException, Request

from meridian_py import dev_jwt
from meridian_py.dev_jwt import Principal

OFFICER_ROLES = ("admin", "operator")
READ_ROLES = ("admin", "operator", "auditor")
FILE_ROLES = ("admin", "operator", "taxpayer")


def validate_auth_config() -> None:
    """Fail closed at boot in keycloak/prod mode without OIDC config."""
    dev_jwt.validate_auth_config()


def _principal_claim_tins(principal: Principal) -> set[str]:
    tins = set()
    tin = principal.get("tin", "")
    if tin:
        tins.add(tin)
    for t in principal.get("tins", []) or []:
        tins.add(t)
    return tins


def tin_in_scope(principal: Principal, tin: str) -> bool:
    """Staff/auditor roles are cross-TIN; any other role (taxpayer) may only
    touch TINs carried in the verified token claims."""
    if set(READ_ROLES) & set(principal.roles):
        return True
    return tin in _principal_claim_tins(principal)


def require_tin_scope(principal: Principal, tin: str) -> None:
    if not tin_in_scope(principal, tin):
        raise HTTPException(
            status_code=403,
            detail=f"TIN {tin!r} outside caller scope")


async def require_filer(request: Request) -> Principal:
    """Filing write/read: taxpayer (own TIN), operator or admin."""
    return await dev_jwt.require_roles(*FILE_ROLES)(request)


async def require_officer(request: Request) -> Principal:
    """Statutory actions: assessments, objection decisions, clocks."""
    return await dev_jwt.require_roles(*OFFICER_ROLES)(request)


async def require_reader(request: Request) -> Principal:
    """Read access: staff + auditor + taxpayer (taxpayer TIN-scoped)."""
    return await dev_jwt.require_roles(*READ_ROLES, "taxpayer")(request)
