# Vendored coverage matrix — SYNC NOTE

Source of truth: meridian-rule-packs repo `coverage/*.yaml` (LCE SPEC §1/§5).
This copy exists so services resolve statute citations dev-standalone (SPEC §1.3
offline-resilience), mirroring the embedded-pack pattern in `packs/`.

Override at runtime with env `LCE_COVERAGE_DIR` pointing at the canonical
checkout; the resolver loads read-only and a missing/unreadable file degrades
to empty `statute_sections`, never an error (SPEC §5.1 rule b).

`citation_kind` stays `secondary` for WHT/NTA rows until CTC verification
(owned by the registry workstream). Some rows may carry `ctc:` fields added by
the registry workstream — consumers MUST tolerate unknown fields.

Synced from lce-rulepacks coverage @ feature/citation-chain session (2026 audit).
