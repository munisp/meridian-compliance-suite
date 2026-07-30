"""WORM evidence per corrected record (SPEC 3 T3): calls core audit-evidence
(POST /v1/evidence) when AUDIT_EVIDENCE_URL is set; otherwise a local WORM
file store (write-once, sha256-chained) is the dev fallback."""

from __future__ import annotations

import hashlib
import json
import os
import time
from pathlib import Path

import httpx

AUDIT_EVIDENCE_URL = os.environ.get("AUDIT_EVIDENCE_URL", "").rstrip("/")
WORM_DIR = Path(os.environ.get("DATA_DIR", "/tmp/meridian-rev360")) / "worm"


def _sha256(payload: bytes) -> str:
    return hashlib.sha256(payload).hexdigest()


def store_evidence(subject: str, kind: str, payload: dict) -> dict:
    """Persist an immutable evidence object; returns the evidence record."""
    blob = json.dumps({"subject": subject, "kind": kind, "payload": payload},
                      sort_keys=True).encode()
    digest = _sha256(blob)
    if AUDIT_EVIDENCE_URL:
        try:
            resp = httpx.post(f"{AUDIT_EVIDENCE_URL}/v1/evidence", timeout=4.0,
                              json={"subject": subject, "kind": kind,
                                    "sha256": digest, "payload": payload})
            if resp.status_code in (200, 201):
                out = resp.json()
                out["via"] = "audit-evidence-api"
                return out
        except Exception:
            pass  # fall back to local WORM store
    return _local_worm(subject, kind, digest, payload)


def _local_worm(subject: str, kind: str, digest: str, payload: dict) -> dict:
    """Local WORM fallback: one immutable file per evidence object, chained
    by the hash of the previous object (tamper-evident sequence)."""
    WORM_DIR.mkdir(parents=True, exist_ok=True)
    chain_file = WORM_DIR / "chain.head"
    prev = chain_file.read_text().strip() if chain_file.exists() else "GENESIS"
    evidence_id = f"ev-{digest[:16]}"
    record = {
        "id": evidence_id, "subject": subject, "kind": kind,
        "sha256": digest, "prev_hash": prev,
        "worm_uri": f"worm://local/{evidence_id}",
        "immutable": True, "created_at": time.strftime(
            "%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "payload": payload, "via": "local-worm-file-store",
    }
    path = WORM_DIR / f"{evidence_id}.json"
    if path.exists():
        raise FileExistsError(f"WORM object {evidence_id} already exists (immutable)")
    # write-once: exclusive create
    with open(path, "x") as fh:
        json.dump(record, fh, indent=2, sort_keys=True)
        fh.flush()
        os.fsync(fh.fileno())
    chain_file.write_text(digest)
    return record


def verify_evidence(evidence_id: str) -> dict:
    """Verify a local WORM object's integrity."""
    path = WORM_DIR / f"{evidence_id}.json"
    if not path.is_file():
        return {"id": evidence_id, "found": False, "valid": False}
    record = json.loads(path.read_text())
    blob = json.dumps({"subject": record["subject"], "kind": record["kind"],
                       "payload": record["payload"]}, sort_keys=True).encode()
    ok = _sha256(blob) == record["sha256"]
    return {"id": evidence_id, "found": True, "valid": ok,
            "sha256": record["sha256"], "worm_uri": record["worm_uri"]}
