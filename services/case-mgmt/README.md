# case-mgmt (T13-practitioner) — practitioner case management

Matters CRUD, documents with privilege flags, deadlines + escalation, client
portal API, Permify-style relations (matter#client, matter#counsel,
doc#privileged) with dev file-backed checker, evidence-pack builder via core
WORM API (local content-addressed fallback), wf-case-intake/lifecycle/deadlines
workflow functions, and a background deadline watch with notifications (core
notification svc API, log fallback).

## Run (dev, zero external deps)
```sh
go build . && ./case-mgmt          # PORT=8113, AUTH_MODE=dev
```

## Config: PORT, AUTH_MODE, MERIDIAN_DEV_JWT_SECRET, DATA_DIR, AUDIT_EVIDENCE_URL, NOTIFICATION_SVC_URL

## API
POST /v1/matters · GET /v1/matters[/{id}] · PATCH /v1/matters/{id} ·
POST /v1/matters/{id}/documents (multipart or JSON) · GET /v1/matters/{id}/documents ·
GET /v1/documents/{id}[/content] · POST /v1/matters/{id}/deadlines ·
GET /v1/deadlines · PATCH /v1/deadlines/{id} · GET /v1/portal/matters[/{id}] ·
POST /v1/relations/check|grant|revoke · GET /v1/relations ·
POST /v1/matters/{id}/evidence-pack · GET /v1/workflows · POST /v1/workflows/{name}/run

## Honesty tags
- REAL: relation-checked access (clients cannot read privileged docs),
  sha256 document addressing, deadline escalation (<72h warn, overdue escalate),
  evidence pack assembly + WORM receipt, workflow runner with retries.
- SIMULATED: Permify replaced by dev file-backed checker (same tuple model);
  WORM falls back to local content-addressed read-only files; notifications
  fall back to structured logs; blob storage is local disk.


## Auth (fail-closed contract)

`AUTH_MODE=dev` (default): HS256 Bearer tokens (`MERIDIAN_DEV_JWT_SECRET`)
plus an allowlisted `X-Dev-Role` header (`admin|operator|auditor`).
`AUTH_MODE=keycloak`: RS256 Bearer tokens verified against the realm JWKS
(`KEYCLOAK_ISSUER` / `KEYCLOAK_AUDIENCE` / `KEYCLOAK_JWKS_URL`; iss/aud/exp
enforced, keys cached with refresh-on-unknown-kid). **Fail closed:** a
keycloak deployment missing its OIDC configuration refuses to start — there
is no silent fallback to dev auth, and `X-Dev-Role` is ignored in keycloak
mode.
