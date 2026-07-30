# pos-vat (T6) — POS VAT service

POS receipt ingest API (in-mem hot path, optional Redis hot cache, durable
embedded fallback), VAT baskets (`standard_75`/`zero_rated`/`exempt`) from
rp-vat-* packs, capture-time state/LGA attribution, federal/state attribution
switch (`rp-vat-attribution-mode`) with `dual_shadow` mode, store-and-forward
spool + spool-drain, settlement recon posting to the VAT remittance ledger
(300), variance detection, B2C reporting, certification runs, and the wf-vat-*
workflow set (in-process dev runner mirroring temporal-sdkx semantics).

## Run (dev, zero external deps)
```sh
go build . && ./pos-vat            # PORT=8106 default, AUTH_MODE=dev
curl -X POST localhost:8106/v1/receipts -H 'X-Dev-Role: operator' -d '{
  "tenant_id":"t1","merchant_tin":"12345678-0001","terminal_id":"P1",
  "receipt_no":"R1","lat":6.52,"lon":3.37,
  "lines":[{"sku":"A","qty_milli":1000,"unit_price_kobo":10000,"category":"electronics"}]}'
```

## Config (env)
PORT, AUTH_MODE, MERIDIAN_DEV_JWT_SECRET, GEO_SVC_URL, LEDGER_SVC_URL,
RP_REGISTRY_URL, REDIS_URL, DATA_DIR, ATTRIBUTION_MODE, EVENT_BUS, TIN_HMAC_KEY.

## API
GET /healthz /readyz · POST /v1/receipts · GET /v1/receipts[/{id}] ·
GET /v1/spool · POST /v1/spool/drain · POST /v1/settlement/recon ·
GET /v1/settlement/recon · GET /v1/variance · POST /v1/b2c/report ·
POST /v1/cert-run · GET /v1/attribution/mode · GET /v1/packs · GET /v1/events ·
GET /v1/workflows · POST /v1/workflows/{name}/run

## Honesty tags (simulated vs real)
- REAL: basket classification from packs, VAT computation (integer kobo),
  attribution modes incl. dual_shadow, spool/drain, variance, cert-run,
  dev TigerBeetle-semantics ledger (pending/post/void, float constraint),
  dev JWT auth, event envelope + file outbox.
- SIMULATED: geo attribution uses embedded coarse state bboxes + LGA centroids
  when GEO_SVC_URL unset (production: geo-rs point-in-polygon); durable store
  is an embedded append-log (SQLite stand-in — build sandbox has no module
  proxy; interface is swap-ready); event bus is in-process + file outbox.
