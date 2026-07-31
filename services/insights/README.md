# insights — Part B innovation service (I8–I12, I14)

FastAPI microservice for compliance intelligence over the Meridian data plane.

| Endpoint | Innovation | Status |
|---|---|---|
| POST /v1/insights/circularity | I8 invoice-chain circularity detector (A→B→C→A VAT-padding loops; in-process graph, `FalkorDBGraph` interface) | REAL (FalkorDB backend SIMULATED until `FALKORDB_URL`) |
| POST /v1/insights/benchmarks | I9 sector benchmark outlier report (per-sector tax/turnover distribution, ≥2σ flags) | REAL |
| POST /v1/insights/penalties | I10 penalty/interest engine (NTAA late filing + late payment + MPR interest, effective-date aware) | REAL (MPR table seeded; override `MPR_TABLE_JSON`) |
| POST /v1/insights/reminders | I11 filing-deadline predictive reminders, pack-driven (rp-fmt-federal), emits `nrs.reminders.due.v1` | REAL (Kafka emission SIMULATED — events returned, not produced) |
| POST /v1/insights/explain | I12 anomaly explainability cards from validator traces (+ ML feature-store hook) | REAL for rules (feature-store SIMULATED until `FEATURE_STORE_URL`) |
| POST /v1/insights/fx/convert | I14 FX validation (CBN-rate interface, daily table, conversion audit trail) | REAL with static table (live CBN SIMULATED until `CBN_FX_URL`) |

Run: `uvicorn app.main:app --port 8088` · Tests: `pytest -q`
