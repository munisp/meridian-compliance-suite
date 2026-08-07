# str-filing — STR / AML regulatory-filing pipeline

Suspicious Transaction Report (STR) filing to NFIU for the Meridian NRS
platform. Closes assurance waveC gaps **C4** (no STR filing queue), **C7**
(no durable STR DLQ), **C8** (no requeue path), **C10** (no STR audit
trail). Modelled on the proven Odoo `nrs.submission_log` retry/requeue
pattern (`integrations/odoo/meridian_nrs_einvoice/models/nrs_submission_log.py`).

## Architecture

```
kyc-engine (PEP/EDD, sanctions hits)          NFIU
        |  nrs.aml.str.created (Kafka)           ^
        v                                        | HTTP submit (+ idempotency key)
 str-filing: intake -> str_filings (Postgres) -> retry worker -> filed
                          |  pending->submitting->filed
                          |       \->failed (exp backoff)\->dlq --requeue(RBAC)-->pending
                          +-> audit: WORM record per transition (audit-evidence)
```

- **Intake**: REST `POST /v1/str` and Kafka topic `nrs.aml.str.created`
  (kafka-python consumer, enabled when `KAFKA_BOOTSTRAP_SERVERS` is set;
  HTTP-only otherwise). Both paths share one idempotent intake function.
  Event contract: `{tenant_id, idempotency_key, subject_ref, report_type,
  payload, actor}`.
- **Queue**: durable `str_filings` table (Postgres via `STR_DATABASE_URL`;
  SQLite under `DATA_DIR` for zero-dep dev). State machine
  `pending -> submitting -> filed`, `-> failed` (retryable, exponential
  backoff `STR_RETRY_BASE_SECONDS * 2^(n-1)`, capped by
  `STR_RETRY_MAX_BACKOFF_SECONDS`), `-> dlq` after `STR_MAX_ATTEMPTS`
  (default 5) or immediately on NFIU 4xx. Idempotency: unique
  `(tenant_id, idempotency_key)` — duplicate intake returns HTTP 200 with
  the existing record.
- **NFIU adapter**: interface `NFIUClient`. `HTTPNFIUClient`
  (transport=HTTP, REAL; `NFIU_BASE_URL`/`NFIU_API_KEY`) is the default and
  the **only** adapter allowed in prod profile. `SimNFIUClient`
  (transport=**SIM**, SIMULATED — dev/test/runbook only, refused in prod)
  behind `STR_NFIU_ADAPTER=sim`.
- **Retry worker**: background thread (`STR_WORKER_INTERVAL_SECONDS`,
  disable with `STR_WORKER_ENABLED=false`); no row is ever deleted or
  dropped — outage only moves rows between pending/failed/dlq.
- **Requeue**: `POST /v1/str/{id}/requeue` (dlq→pending, attempts reset).
  RBAC-gated via the case-mgmt Permify checkRel pattern: `PERMIFY_URL` set
  → live check `str_filing:<id>#requeue@user:<sub>`; unset → dev role
  fallback (`admin`/`compliance-officer`); prod profile + no Permify fails
  closed at startup.
- **Metrics** (`GET /metrics`): `str_dlq_depth` (gauge, per tenant),
  `str_submission_errors_total` (counter, kind=unavailable|rejected),
  `str_filed_total` (counter).
- **Audit**: one WORM-style record per state transition with forensic
  fields `actor, timestamp, str_id, tenant_id, old_status, new_status,
  str_hash`. `AUDIT_EVIDENCE_URL` set → sealed to the core audit-evidence
  service (`POST /v1/evidence`, same as case-mgmt); else local append-only
  hash-chained JSONL (`DATA_DIR/str_audit.jsonl`, dev fallback).

## REAL/SIM honesty

| Component | Tag | Notes |
|---|---|---|
| Queue, retry, DLQ, requeue, idempotency, metrics, audit chain | REAL | Postgres-durable; tested |
| HTTPNFIUClient | REAL | Live NFIU connectivity is runbook territory |
| SimNFIUClient, `/v1/str/sim/outage` | **SIM** | In-process; refused in prod profile |
| Kafka consumer | REAL (env-gated) | Needs a broker; HTTP intake works without |

## Runbook — NFIU outage (waveC C4 RUNBOOK)

1. **Detect/declare outage**: NFIU unreachable or 5xx. To rehearse, point
   the adapter at a blackhole (`NFIU_BASE_URL=http://127.0.0.1:1`) or, on a
   SIM deployment, `POST /v1/str/sim/outage {"available": false}`.
2. **Submit N STRs** (normal intake: Kafka events or `POST /v1/str`).
3. **Observe**: worker attempts fail retryably; rows cycle
   pending→submitting→failed with exponential backoff; after
   `STR_MAX_ATTEMPTS` they land in `dlq`. Check
   `GET /v1/str/dlq/depth` — depth == number of exhausted filings; watch
   `str_dlq_depth` / `str_submission_errors_total` in Prometheus. **No
   filing is lost**: every row remains in `str_filings` with `last_error`.
4. **Restore NFIU** (fix `NFIU_BASE_URL` / SIM
   `POST /v1/str/sim/outage {"available": true}`).
5. **Drain the DLQ**: an officer with Permify `str_filing#requeue` (or dev
   role `compliance-officer`/`admin`) calls `POST /v1/str/{id}/requeue` per
   filing (list them with `GET /v1/str?status=dlq&tenant_id=...`). Rows go
   back to `pending` and the worker files them.
6. **Assert exactly-once**: each STR carries a unique
   `(tenant_id, idempotency_key)`; the NFIU adapter also sends it as
   `X-Idempotency-Key`. Verify `GET /v1/str/{id}` shows `filed` with an
   `nfiu_reference`, and `str_filed_total` increased by exactly N.
7. **Audit**: every transition above (including each requeue, with the
   officer's identity as `actor`) is a WORM audit record — export from
   audit-evidence by `kind=str-filing-transition`.

## Dev run

```sh
pip install -r requirements.txt
STR_NFIU_ADAPTER=sim uvicorn app.main:app --port 8150
pytest tests/            # 12 tests, full lifecycle
```
