# Meridian Compliance Suite

Nigeria tax compliance services (e-invoicing / WHT / TP-CbCR) built to the Meridian platform DESIGN-CONTRACT.

## Layout

```
├── packages/
│   ├── shared/
│   │   ├── rulepack/      # rp-* pack loader + embedded fallback (Go) + Python mirror (packages/py)
│   │   ├── envelope/      # transactional-outbox envelope bus (Go inproc impl)
│   │   └── util/          # ULID, RFC3339, problem+json, money helpers
│   ├── devjwt/            # HS256 dev JWT + fail-closed keycloak mode (Go)
│   ├── otelx/             # OTel middleware: http.route + tenant.id + W3C propagation
│   └── py/                # meridian_py: Python mirror of the shared substrate
├── core/                  # (this suite) authnz, api-gateway, rules-engine,
│   │                      #   registry, ledger-connector, audit-evidence, tin-graph
├── services/
│   ├── einvoicing/      # Go — T1/T2 (e-invoicing, NRS rail, VAT summary, API keys)
│   ├── rev360/          # Python FastAPI — T3
│   ├── wht/             # Python FastAPI — T7 (WHT 2024 engine, credits, certificates)
│   ├── tp-cbcr/         # Python FastAPI — T8
│   ├── filings/         # Python FastAPI — F1-F4 periodic filings (VAT-002,
│   │                    #   PAYE schedules + H1, CIT, assessment/objection lifecycle)
│   ├── str-filing/      # Python FastAPI — STR/CTR AML filing pipeline (NFIU)
│   ├── insights/        # Python FastAPI — I8-I12/I14 compliance intelligence
│   │                    #   (circularity, benchmarks, explanations, FX)
│   ├── pos-vat, etr, vasp-carf, case-mgmt/   # (Agent B scope — see part B)
│   └── ...
├── portals/             # (Agent B scope)
└── docker-compose.yml   # optional dev stack (einvoicing, rev360, wht,
                         #   tp-cbcr, str-filing — see "Docker compose" below)
```

## Substrate contracts

### rulepack (rp-* packs)

Every statutory rule lives in a versioned rule pack (`packages/shared/rulepack/packs/<id>/<version>.yaml`).
Services load registry-first (`RULE_PACK_REGISTRY_URL`), embedded-fallback, and **fail closed in prod**
when a pack is unavailable. `GET /v1/pack` per service exposes the active pack metadata;`subject_to_regazette`
is surfaced per the platform's as-passed/as-gazetted doctrine.

### envelope bus

All inter-service events travel as envelope `{id, type, source, time, tenant_id, trace_id, rule_pack_version, data}`.
Go: `packages/shared/envelope` in-process impl for dev; Python: `meridian_py.envelope`. Kafka is the prod transport.

### money

Integer kobo everywhere; round-half-up at tax points (rp-mbs-business-rules). Floats never persist.

### auth

`devjwt.Middleware` (Go) / `meridian_py.dev_jwt.AuthDep` (Python). `AUTH_MODE=dev` accepts HS256 dev tokens +
`X-Dev-Role`; `AUTH_MODE=keycloak` verifies RS256 via JWKS and fails closed without `KEYCLOAK_ISSUER`.

### otel

`init_otel(app)` (fail-soft) + `TenantBaggageMiddleware`; Go services use `otelx.Middleware`.
Spans carry `tenant.id` + low-cardinality `http.route`.

## Build & test

```bash
# Go
cd services/einvoicing && go test ./...
cd packages/... && go test ./...

# Python (single venv at repo root recommended)
pip install -r services/rev360/requirements.txt -r services/wht/requirements.txt \
            -r services/tp-cbcr/requirements.txt
(cd services/rev360 && pytest) && (cd services/wht && pytest) && \
  (cd services/tp-cbcr && pytest)

# filings / str-filing / insights (zero-deps dev mode, SQLite/in-mem stores)
(cd services/filings && pytest) && (cd services/str-filing && pytest) && \
  (cd services/insights && pytest)
```

Run filings or insights standalone (dev profile, no external deps):

```bash
(cd services/filings  && uvicorn app.main:app --port 8160)   # F1-F4 filings API
(cd services/insights && uvicorn app.main:app --port 8170)   # I8-I12 insights API
```

## Docker compose (optional dev stack)

`docker-compose.yml` brings up einvoicing, rev360, wht, tp-cbcr and
str-filing with their dev data volumes. **services/filings and
services/insights are intentionally not in the compose stack**: both are
self-contained FastAPI apps with embedded dev stores — run them standalone
as shown above (or `uvicorn app.main:app` from each directory); add compose
entries alongside the other `python:3.12-slim` services if you want them in
the stack.

## Integration test harness

```bash
tools/test-harness/run.sh   # boots the stack, runs smoke suite against live endpoints
```

## Audit evidence

WORM append-only audit store (`core/audit-evidence`), hash-chained; every
money/statutory transition records an evidence entry (see each service's docs).
