# vasp-carf (T10) — VASP cost basis + OECD CARF

VASP trade/transfer ingest, cost-basis engine (FIFO + weighted-average), FMV
snapshot cache, ring-fence calculation (rp-nta-digital-assets), per-asset
gain/loss accounting ledger (NOT payments), OECD CARF XML message builder with
correction loop (OECD1/OECD2/OECD3 DocSpec), and reg-watch gate enforcement —
`carf.transmit_enabled` / `carf.gate.changed` refuse transmission while closed
(fail-safe default), via reg-watch API with local gate-file fallback.
rp-carf-schema validation, rp-nta-vasp-duties evaluation.

## Run (dev, zero external deps)
```sh
go build . && ./vasp-carf          # PORT=8110, AUTH_MODE=dev
```

## Config: PORT, AUTH_MODE, MERIDIAN_DEV_JWT_SECRET, RP_REGISTRY_URL, REG_WATCH_URL, DATA_DIR

## API
POST /v1/trades · GET /v1/trades · POST /v1/transfers · GET /v1/transfers ·
GET /v1/costbasis/{asset}?method=fifo|wac · POST /v1/fmv/snapshots ·
GET /v1/fmv/{asset} · POST /v1/ringfence/compute · GET /v1/gains|/v1/ledger ·
POST /v1/carf/build · GET /v1/carf/messages[/{id}[/xml]] ·
POST /v1/carf/messages/{id}/correct · POST /v1/carf/transmit ·
GET /v1/gates · POST /v1/gates/{id}/flip (admin) · POST /v1/duties/evaluate · GET /v1/packs

## Honesty tags
- REAL: FIFO/WAC basis with lot relief, insufficient-basis rejection,
  transfer-out-at-FMV disposal, ring-fence with carry-forward, CARF XML
  (urn:oecd:ties:carf:v1), correction loop with CorrMessageRefId, pack-driven
  schema validation + duties, fail-safe gates.
- SIMULATED: transmit performs no real OECD channel I/O (dev: status flip +
  envelope log); reg-watch consumed via API when configured, local gate file
  otherwise; durable store is embedded append-log (SQLite stand-in).
