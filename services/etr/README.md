# etr (T9) — Pillar Two ETR computation engine

Full GloBE ETR engine: constituent entity ingest, net income + covered taxes
per jurisdiction, dual-basis adjustments (NTA + OECD GloBE), substance-based
income exclusion (SBIE transition carve-out % tables), ETR per jurisdiction,
top-up %, IIR allocation down the ownership chain (UPE + POPE ordering), CFC
pushdown pool, QDMTT upgrade flag, audit-defensible step trace per computation,
GIR XML + filing-pack JSON builder, rp-etr-nta/rp-etr-scope/rp-etr-cfc/
rp-globe-oecd/rp-gir-schema pack loading (rp-registry API + embedded fallback),
wf-etr-compute / wf-globe-extract / wf-filingpack-build workflows.

## Run (dev)
```sh
python3 -m venv .venv && .venv/bin/pip install -r requirements.txt
.venv/bin/python -m app.main          # PORT=8109, AUTH_MODE=dev, sqlite at DATA_DIR/etr.db
# or: .venv/bin/uvicorn app.main:app --port 8109
```

## API
POST /v1/etr/groups · POST /v1/etr/entities · GET /v1/etr/entities ·
POST /v1/etr/compute · GET /v1/etr/computations[/{id}[/trace|/gir.xml|/filing-pack.json]] ·
GET /v1/packs · GET /v1/workflows · POST /v1/workflows/{name}/run ·
POST /v1/dev-token (dev JWT mint for the portal)

## Honesty tags
- REAL: full computation pipeline (scope, exclusions, dual-basis adjustments,
  jurisdictional blending, CFC pushdown, SBIE, ETR/top-up, QDMTT, IIR with
  POPE effective-ownership), step trace with rule refs + pack versions,
  sha256 computation digest, GIR XML, filing-pack JSON, sqlite persistence.
- SIMULATED: EUR 750m threshold expressed as NGN-kobo dev equivalent in
  rp-etr-scope; qdmtt_upgrade passed per-request (prod: reg-watch gate);
  GIR follows rp-gir-schema required sections (final OECD schema subject to
  regazette — packs carry subject_to_regazette: true).
