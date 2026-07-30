# Meridian Compliance Suite (Market Zone)

Nigerian NRS tax platform — compliance plane. Implements T1/T2 (e-invoicing &
MBS pre-clearance), T3 (Rev360 reconciliation workbench), T7 (WHT Regulations
2024) and T8 (transfer pricing / CbCR). Consumes core contracts from
meridian-core-platform — **pinned to core contracts v1** (schema types copied
locally per SPEC §3; no cross-repo source imports).

## Layout

```
compliance-suite/
├── packages/
│   ├── shared/          # Go module: envelope (SPEC §1.1), devjwt (§1.3),
│   │                    #   rulepack loader/evaluator + embedded rp-* packs (§1.4)
│   └── py/meridian_py/  # Python package with the same helpers (pip install -e)
├── services/
│   ├── einvoicing/      # Go — T1/T2
│   ├── rev360/          # Python FastAPI — T3
│   ├── wht/             # Python FastAPI — T7
│   ├── tp-cbcr/         # Python FastAPI — T8
│   ├── pos-vat, etr, vasp-carf, case-mgmt/   # (Agent B scope — see part B)
│   └── ...
├── portals/             # (Agent B scope)
└── docker-compose.yml   # optional dev stack
```

## Conventions (SPEC §1)

- Event envelope JSON `{id(ulid), type, source, time, tenant_id, trace_id,
  rule_pack_version, data}`; dev bus is in-process (`EVENT_BUS=inproc`);
  producers write to a durable file outbox (replay queue).
- Auth: Bearer HS256 JWT (`MERIDIAN_DEV_JWT_SECRET`, claims `sub`, `roles`,
  `tenant_id`); `AUTH_MODE=dev` also accepts `X-Dev-Role: admin|operator|auditor`.
- Errors are RFC7807 `application/problem+json`. Money is **integer kobo only**.
- Every service: `GET /healthz`, `GET /readyz`, env config, zero external
  deps in dev mode.
- Rule packs (`rp-*`) are YAML per SPEC §1.4, embedded at
  `packages/shared/rulepack/packs/` with `subject_to_regazette: true` and
  provenance. Services try the core rules-engine / rp-registry first
  (`RULES_ENGINE_URL`, `RP_REGISTRY_URL`), then fall back to the embedded
  copies.

## services/einvoicing (Go) — T1/T2

Canonical invoice model → **real UBL 2.1 / Peppol BIS Billing 3.0 XML**,
validation against **rp-ubl-bis + rp-mbs-business-rules** (rules-engine API,
embedded fallback), **CSID ed25519 signing** (dev keys persisted under
`DATA_DIR`, seedable via `CSID_SEED_HEX`), **MBS pre-clearance** client with
adapter interface + **sandbox simulator** (returns IRN `IRN-<tin>-<date>-<seq>`
+ verifiable ed25519 cryptographic stamp), **B2C real-time reporter**,
**durable replay queue** (JSONL outbox + replay endpoint), **multi-APP
pluggable router** (`APP_ROUTES=tenant=app,...`), ERP adapters: **REST + CSV
real**, **SAP OData v4 adapter** (payload mapping real; live fetch configured
via `SAP_ODATA_URL`), **offline queue + idempotent resubmission**
(`Idempotency-Key` header), and **wf-mbs-preclearance** (validate → sign →
UBL → MBS submit → record) on an in-process runner with Temporal-style
retry/step semantics (`TEMPORAL_URL` unset; core temporal-sdkx wiring point).

REST surface (SPEC §3):
`POST /v1/invoices` (JSON | `text/csv` | `?adapter=sap-odata`),
`POST /v1/invoices/{id}/preclear`, `GET /v1/invoices/{id}` (`?format=ubl`),
`POST /v1/b2c/report`, `GET /v1/replay`, plus
`POST /v1/replay/{seq}`, `GET /v1/workflows`, `GET /v1/apps`,
`GET /v1/csid/public-key`.

```bash
export PATH=$HOME/sdk/go/bin:$PATH
cd services/einvoicing && go run .            # :8110
curl -X POST :8110/v1/invoices -H 'X-Dev-Role: operator' \
     -H 'Content-Type: application/json' -d @invoice.json
```

## services/rev360 (Python FastAPI) — T3

Legacy **CSV/XML ingest**, **Rev360-view simulator** (deterministic NRS-side
dataset, *simulated*), **defect-class rules engine** with the full taxonomy —
`wrong_assessment`, `blocked_tcc`, `unrecognised_remittance`,
`duplicate_payment`, `tin_mismatch` — with severity rubric, **case ticketing
CRUD**, **consultant OIDC dev SSO** (`GET /v1/sso/login` issues a dev JWT),
**controlled ETL endpoints** (extract → staging → clean with key controls),
and **WORM evidence per corrected record** on case resolution (calls core
audit-evidence `POST /v1/evidence` when `AUDIT_EVIDENCE_URL` set; otherwise a
hash-chained local WORM file store).

```bash
cd services/rev360 && uvicorn app.main:app --port 8120
```

## services/wht (Python FastAPI) — T7

**WHT Regulations 2024 engine** — deduction at the **earlier of payment or
settlement**, **no-TIN double rate**, **NIN acceptable** for individuals,
**≤ ₦2m/month small-company carve-out** for valid-TIN suppliers,
**direct-debit / broker / manufacturer / import exemptions** — all evaluated
from the **rp-wht-2024** pack (rp-registry/rules-engine first, embedded
fallback). **Vendor TIN validation** via tin-graph (`TIN_GRAPH_URL`) with a
local 13-digit validator fallback. **WHT credit ledger** (SQLAlchemy; SQLite
dev, Postgres via `DATABASE_URL`), **remittance file generation (CSV + XML)**,
**wf-wht-remit-schedule** (collect → aggregate → generate → post credits →
mark remitted).

REST (SPEC §3): `POST /v1/wht/evaluate`, `POST /v1/wht/remit-file`,
`GET /v1/wht/credits/{vendor_tin}` plus deductions CRUD, credit application,
vendor verification, workflow runs.

```bash
cd services/wht && uvicorn app.main:app --port 8130
```

## services/tp-cbcr (Python FastAPI) — T8

Entity/transaction **graph ingest API**, **OECD CbCR XML generator** (real
namespaced XML per the OECD CbC XML Schema v2.0 structure:
`urn:oecd:ties:cbc:v2` + STF DocSpec), **master file + local file assembly**
(structured JSON + HTML render per OECD Action 13 section structure),
**connected-party interest deductibility calculator** (30%-of-EBITDA
limitation + 5-year carryforward from **rp-tp-2018**), **swappable-pack
mechanism** (`PUT /v1/packs/pin/{tenant}` pins a pack version per tenant),
and a **multi-currency FX table** with integer conversion.

```bash
cd services/tp-cbcr && uvicorn app.main:app --port 8140
```

## Build & test

```bash
# Go (einvoicing + shared packages)
export PATH=$HOME/sdk/go/bin:$PATH
go build ./... && go vet ./... && go test ./...

# Python services
python3 -m venv .venv && . .venv/bin/activate
pip install -e packages/py
pip install -r services/rev360/requirements.txt -r services/wht/requirements.txt \
            -r services/tp-cbcr/requirements.txt
(cd services/rev360 && pytest) && (cd services/wht && pytest) && \
  (cd services/tp-cbcr && pytest)
```

## Honesty tags (what is simulated)

| Component | Status |
|---|---|
| MBS rail | **SIMULATED** — full sandbox (IRN + ed25519 stamp) behind `MBSClient`; real rail via `MBS_BASE_URL` |
| Rev360 (NRS-side) view | **SIMULATED** — deterministic dataset generator, the system under reconciliation |
| rules-engine / rp-registry | Real local evaluator; core API used when URLs set |
| tin-graph TIN verification | Local 13-digit validator; core API when `TIN_GRAPH_URL` set |
| audit-evidence WORM | Local hash-chained WORM store; core API when `AUDIT_EVIDENCE_URL` set |
| Temporal | In-process runner with same step/retry semantics; `TEMPORAL_URL` is the core-sdkx wiring point |
| CSID / MBS keys | Real ed25519 crypto, **dev keys** (persisted/seeded) |
| Consultant SSO | Dev OIDC simulator (`/v1/sso/login`); prod = Keycloak OIDC JWKS |
| FX rates | Dev static table, overridable via API |

## Part B areas (owned by another agent)

`services/pos-vat` (T6), `services/etr` (T9), `services/vasp-carf` (T10),
`services/case-mgmt` (T13p) and `portals/compliance-portal` are built by the
part-B agent on branch `build/compliance-b`. This README covers them only
pointers-wise; see their own docs.
