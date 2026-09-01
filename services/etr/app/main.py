"""Meridian T9 ETR service — Pillar Two / GloBE computation engine API."""
from __future__ import annotations

import base64
import hashlib
import hmac
import json
import os
import time
from contextlib import asynccontextmanager
from typing import Any

import uvicorn
from fastapi import Depends, FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse, PlainTextResponse, Response

from .engine import compute as engine_compute
from .gates import qdmtt_upgrade_armed
from .gir import build_filing_pack, build_gir_xml
from .models import ComputeRequest, ConstituentEntity, Group
from .packs import PackSet, load_packset
from .store import Store
from .workflows import WORKFLOWS


class Ctx:
    def __init__(self) -> None:
        self.store = Store()
        self.packs: PackSet = load_packset()


ctx = Ctx()


@asynccontextmanager
async def lifespan(app: FastAPI):
    # FAIL CLOSED (audit fix PB-6): AUTH_MODE=keycloak without KEYCLOAK_ISSUER
    # refuses to boot instead of silently dropping to the dev auth path.
    if os.environ.get("AUTH_MODE") == "keycloak" and not os.environ.get("KEYCLOAK_ISSUER"):
        raise RuntimeError(
            "AUTH_MODE=keycloak but KEYCLOAK_ISSUER is unset; refusing to "
            "start (no dev fallback)")
    yield


app = FastAPI(title="meridian-etr", version="1.0.0", lifespan=lifespan)


# OTel bootstrap (DESIGN-CONTRACT.md): fail-soft, never breaks startup or
# money paths. Instruments FastAPI + outbound httpx/requests; tenant.id is
# stamped on the active span + baggage. Authz/tenant guards untouched.
from meridian_py.otel import TenantBaggageMiddleware, init_otel

init_otel(app)
app.add_middleware(TenantBaggageMiddleware)


# ---------- RFC7807 errors ----------

@app.exception_handler(HTTPException)
async def http_exc(request: Request, exc: HTTPException):
    return JSONResponse(status_code=exc.status_code,
                        media_type="application/problem+json",
                        content={"type": f"https://meridian.ng/problems/{exc.status_code}",
                                 "title": str(exc.detail), "status": exc.status_code})


# ---------- dev auth: HS256 JWT or X-Dev-Role (SPEC §1.3) ----------

def _verify_hs256(token: str, secret: str) -> dict:
    parts = token.split(".")
    if len(parts) != 3:
        raise ValueError("malformed token")
    signing = f"{parts[0]}.{parts[1]}".encode()
    mac = hmac.new(secret.encode(), signing, hashlib.sha256).digest()
    pad = "=" * (-len(parts[2]) % 4)
    if not hmac.compare_digest(mac, base64.urlsafe_b64decode(parts[2] + pad)):
        raise ValueError("bad signature")
    payload = json.loads(base64.urlsafe_b64decode(parts[1] + "=" * (-len(parts[1]) % 4)))
    if payload.get("exp") and time.time() > payload["exp"]:
        raise ValueError("token expired")
    return payload


# ---------- keycloak auth: RS256 via JWKS (HARDENING.md H2) ----------

_jwks_client = None


def _keycloak_enabled() -> bool:
    return os.environ.get("AUTH_MODE") == "keycloak"


def _keycloak_configured() -> bool:
    return bool(os.environ.get("KEYCLOAK_ISSUER"))


def _verify_keycloak(token: str) -> dict:
    """Verify an RS256 Keycloak token against the realm JWKS (PyJWKClient,
    which caches keys and refreshes on unknown kid); validates iss/exp/aud
    and maps realm_access.roles + resource_access[aud].roles into `roles`."""
    global _jwks_client
    import jwt  # PyJWT[crypto], imported lazily so dev mode needs nothing

    issuer = os.environ["KEYCLOAK_ISSUER"].rstrip("/")
    audience = os.environ.get("KEYCLOAK_AUDIENCE", "")
    jwks_url = os.environ.get("KEYCLOAK_JWKS_URL") or f"{issuer}/protocol/openid-connect/certs"
    if _jwks_client is None:
        _jwks_client = jwt.PyJWKClient(jwks_url)
    try:
        key = _jwks_client.get_signing_key_from_jwt(token).key
        payload = jwt.decode(
            token, key, algorithms=["RS256"], issuer=issuer,
            audience=audience or None,
            options={"verify_aud": bool(audience), "require": ["exp", "iss", "sub"]})
    except jwt.PyJWTError as e:
        raise ValueError(str(e))
    roles = list(payload.get("realm_access", {}).get("roles", []))
    if audience:
        roles += payload.get("resource_access", {}).get(audience, {}).get("roles", [])
    payload["roles"] = roles
    return payload


async def auth(request: Request) -> dict:
    hdr = request.headers.get("authorization", "")
    if _keycloak_enabled():
        if not _keycloak_configured():
            # fail closed: misconfigured prod denies every request
            raise HTTPException(401, "unauthorized: AUTH_MODE=keycloak but KEYCLOAK_ISSUER is unset")
        if hdr.startswith("Bearer "):
            try:
                return _verify_keycloak(hdr[7:])
            except ValueError as e:
                raise HTTPException(401, f"unauthorized: {e}")
        raise HTTPException(401, "unauthorized: Bearer JWT required (AUTH_MODE=keycloak)")
    secret = os.environ.get("MERIDIAN_DEV_JWT_SECRET", "meridian-dev-secret")
    if hdr.startswith("Bearer "):
        try:
            return _verify_hs256(hdr[7:], secret)
        except ValueError as e:
            raise HTTPException(401, f"unauthorized: {e}")
    if os.environ.get("AUTH_MODE", "dev") == "dev":
        role = request.headers.get("x-dev-role")
        if role in ("admin", "operator", "auditor"):
            return {"sub": f"dev-{role}", "roles": [role]}
    raise HTTPException(401, "unauthorized: Bearer JWT or X-Dev-Role (dev mode) required")


# ---------- meta ----------

@app.get("/healthz")
async def healthz():
    return {"status": "ok", "service": "etr", "version": "1.0.0"}


@app.get("/readyz")
async def readyz():
    return {"status": "ready", "packs_loaded": len(ctx.packs.packs)}


@app.get("/v1/packs")
async def packs(_: dict = Depends(auth)):
    return {"packs": [{"id": pid, **{k: p.get(k) for k in ("version", "status", "subject_to_regazette")},
                       "source": ctx.packs.sources.get(pid), "rules": len(p.get("rules", []))}
                      for pid, p in sorted(ctx.packs.packs.items())]}


# ---- dev JWT issuer for the portal login (DEV ONLY: unauthenticated token
# minting; disabled unless AUTH_MODE=dev) ----

@app.post("/v1/dev-token")
async def dev_token(body: dict):
    if os.environ.get("AUTH_MODE", "dev") != "dev":
        raise HTTPException(404, "not found")
    secret = os.environ.get("MERIDIAN_DEV_JWT_SECRET", "meridian-dev-secret")
    header = base64.urlsafe_b64encode(json.dumps({"alg": "HS256", "typ": "JWT"}).encode()).rstrip(b"=")
    payload = base64.urlsafe_b64encode(json.dumps({
        "sub": body.get("sub", "dev-user"), "roles": body.get("roles", ["operator"]),
        "tenant_id": body.get("tenant_id", "dev"), "exp": int(time.time()) + 86400}).encode()).rstrip(b"=")
    mac = hmac.new(secret.encode(), header + b"." + payload, hashlib.sha256).digest()
    sig = base64.urlsafe_b64encode(mac).rstrip(b"=")
    return {"token": (header + b"." + payload + b"." + sig).decode()}


# ---------- ingest ----------

@app.post("/v1/etr/groups", status_code=201)
async def put_group(group: Group, _: dict = Depends(auth)):
    ctx.store.put_group(group)
    return {"status": "stored", "group_id": group.id}


@app.post("/v1/etr/entities", status_code=201)
async def put_entities(entities: list[ConstituentEntity], _: dict = Depends(auth)):
    if not entities:
        raise HTTPException(422, "at least one entity required")
    n = ctx.store.put_entities(entities)
    return {"status": "stored", "count": n}


@app.get("/v1/etr/entities")
async def list_entities(group_id: str = "", _: dict = Depends(auth)):
    ents = ctx.store.entities(group_id) if group_id else ctx.store.all_entities()
    return {"entities": [e.model_dump() for e in ents]}


# ---------- compute ----------

@app.post("/v1/etr/compute", status_code=201)
async def run_compute(req: ComputeRequest, _: dict = Depends(auth)):
    group = ctx.store.get_group(req.group_id)
    if not group:
        raise HTTPException(404, f"group {req.group_id} not found")
    entities = ctx.store.entities(req.group_id)
    if not entities:
        raise HTTPException(422, f"group {req.group_id} has no constituent entities")
    # Audit fix B2-#10: QDMTT path is gated SERVER-SIDE by the reg-watch
    # qdmtt_upgrade gate; the client-supplied req.qdmtt_upgrade field is
    # ignored (fail-closed to the IIR residual top-up path).
    armed, _gate_source = qdmtt_upgrade_armed()
    comp = engine_compute(ctx.packs, group, entities, req, qdmtt_armed=armed)
    ctx.store.put_computation(comp)
    return comp.model_dump()


@app.get("/v1/etr/computations")
async def list_computations(group_id: str = "", _: dict = Depends(auth)):
    return {"computations": ctx.store.list_computations(group_id)}


def _get_comp(cid: str):
    comp = ctx.store.get_computation(cid)
    if not comp:
        raise HTTPException(404, f"computation {cid} not found")
    return comp


@app.get("/v1/etr/computations/{cid}")
async def get_computation(cid: str, _: dict = Depends(auth)):
    return _get_comp(cid).model_dump()


@app.get("/v1/etr/computations/{cid}/trace")
async def get_trace(cid: str, _: dict = Depends(auth)):
    comp = _get_comp(cid)
    return {"computation_id": cid, "steps": [s.model_dump() for s in comp.trace]}


@app.get("/v1/etr/computations/{cid}/gir.xml")
async def get_gir(cid: str, _: dict = Depends(auth)):
    comp = _get_comp(cid)
    group = ctx.store.get_group(comp.group_id)
    entities = ctx.store.entities(comp.group_id)
    xml = build_gir_xml(ctx.packs, comp, group, entities)
    return Response(content=xml, media_type="application/xml")


@app.get("/v1/etr/computations/{cid}/filing-pack.json")
async def get_filing_pack(cid: str, _: dict = Depends(auth)):
    comp = _get_comp(cid)
    group = ctx.store.get_group(comp.group_id)
    entities = ctx.store.entities(comp.group_id)
    return build_filing_pack(ctx.packs, comp, group, entities)


# ---------- workflows ----------

@app.get("/v1/workflows")
async def list_workflows(_: dict = Depends(auth)):
    return {"workflows": [{"name": n} for n in WORKFLOWS], "runner": "inproc (TEMPORAL_URL unset)"}


@app.post("/v1/workflows/{name}/run")
async def run_workflow(name: str, params: dict[str, Any] | None = None, _: dict = Depends(auth)):
    fn = WORKFLOWS.get(name)
    if not fn:
        raise HTTPException(404, f"unknown workflow {name}")
    return fn(ctx, params or {})


if __name__ == "__main__":
    uvicorn.run("app.main:app", host="0.0.0.0", port=int(os.environ.get("PORT", "8109")))
