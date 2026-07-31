# einvoicing

Meridian e-invoicing service: canonical kobo-integer invoice model, UBL 2.1 /
Peppol BIS mapping, CSID signing, QR verification codes, MBS pre-clearance,
B2C real-time reporting — now with full **NRS e-Invoicing parity** (official
NRS/Gention API spec).

Honesty tags: **[REAL]** = production-grade implementation in this repo;
**[SIM]** = simulator/adapter skeleton until live endpoints are wired.

## NRS 8-step lifecycle mapping [REAL]

`POST /v1/invoices/nrs` accepts an NRS-schema payload and runs the registered
`wf-nrs-einvoice` workflow. Each step is explicit, recorded on the run, and
individually retryable with backoff:

| # | Step | Implementation |
|---|------|----------------|
| 1 | invoice creation/store | draft persisted to the durable store (`nrs_lifecycle.go`) |
| 2 | IRN generation | `IRN = <InvoiceNumber>-<ServiceID>-<YYYYMMDD>` (`nrs_irn.go`); skipped when the payload carries an IRN |
| 3 | IRN validation | format / uniqueness / structure; client-supplied IRNs are verified and reused (idempotent resubmission) |
| 4 | IRN signage | IRN signed with the taxpayer CSID ed25519 key → feeds the QR payload |
| 5 | invoice schema validation | NRS schema + reference catalogs + rp-ubl-bis/rp-mbs packs; fail-fast with the full NRS-style error list |
| 6 | invoice signage | CSID signature + `signed_core_hash` lock → invoice number and all core fields immutable |
| 7 | transmission | HMAC-SHA256-signed webhooks to the business's registered stakeholders, retried with backoff (`webhooks.go`) |
| 8 | confirmation | reconciliation check against the store → status `confirmed` |

## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| POST | `/v1/invoices` | existing adapter ingestion (REST/CSV/SAP-OData) — unchanged |
| POST | `/v1/invoices/nrs` | NRS-schema ingestion → 8-step lifecycle → `{irn, status, qr, crypto_stamp, steps, invoice}` |
| PATCH | `/v1/invoices/{irn}` | update `payment_status` (PENDING\|PAID\|REJECTED) and/or `reference` only; any other field → **409**; emits `nrs.einvoice.payment_status.v1` with an audit-trail entry |
| POST | `/v1/webhooks` | register a stakeholder webhook `{business_id, url, secret}` (fail-closed in prod: HTTPS + ≥16-byte secret) |
| GET | `/v1/webhooks?business_id=` | registered endpoints (secrets redacted) + delivery history |
| GET | `/v1/invoices/{id}`, `POST /v1/invoices/{id}/preclear`, `GET /v1/invoices/{id}/qr`, `POST /v1/b2c/report`, `GET /v1/replay`, `POST /v1/replay/{seq}`, `GET /v1/workflows`, `GET /v1/apps`, `GET /v1/csid/public-key` | existing — unchanged |

## NRS schema ⇄ canonical model [REAL]

`nrs_schema.go` implements the NRS UBL-shaped JSON schema (`NRSInvoice`) and
lossless boundary conversion:

- **Money**: NRS payloads use decimal NGN floats; the internal model is
  integer kobo. Conversion happens only at the boundary via `NGNToKobo`
  (decimal-space round-half-up, so `245236.28024999998` → `24523628` and
  `0.575` → `58`). Floats are never stored.
- **Tax categories**: `STANDARD_VAT`→`S`/750bps, `ZERO_VAT`→`Z`, `EXEMPT`→`E`
  etc. (`catalogs.go`).
- **Reference catalogs** [REAL]: tax categories, invoice type codes
  (380/381/383/…), payment means codes (10/42/…), ISO 3166-2:NG state codes
  (36 states + FCT), LGA codes (`NG-AB-ANO` style; full Abia/Lagos/FCT sets +
  documented structural extension path), ISO 4217 currencies, HSN/service
  codes. Validation aggregates every violation into an NRS-style error list.

## Immutability & idempotency [REAL]

- After step 6 (invoice signage) the core-field hash (`signed_core_hash`) is
  locked; the confirmation step reconciles it against the store.
- `PATCH /v1/invoices/{irn}` whitelists `payment_status`/`reference`; locked
  field mutation attempts return 409.
- Resubmission of a payload with the same IRN returns the existing invoice
  (`idempotent_replay: true`) — no duplicates.

## Webhooks [REAL]

Per-business stakeholder endpoints, `X-Meridian-Signature` (HMAC-SHA256 hex)
+ `X-Meridian-Event` headers, 3-attempt linear-backoff retry, delivery
history. Dev: `WEBHOOK_SINK=inproc` installs the in-process sink.
Registration is fail-closed when `ENV`/`APP_ENV` indicates production.

## Still simulated [SIM]

- MBS/NRS backend clearance: `SandboxMBS` stamps IRNs locally; the live rail
  activates via `MBS_BASE_URL` (`HTTPMBS`). NRS-side IRN registration and
  clearance responses are simulated until live NRS endpoints are wired.
- Service-ID registry auto-assigns random ids in dev; production injects the
  NRS-issued 8-char id at integrator onboarding.

## Events

`nrs.einvoice.transmitted.v1`, `nrs.einvoice.confirmed.v1`,
`nrs.einvoice.payment_status.v1` (new) plus the existing
`nrs.mbs.*.v1` topics — all via the transactional outbox.
