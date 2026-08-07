"""STR audit trail — one WORM-style record per state transition.

Follows the platform audit-evidence approach used by case-mgmt
(services/case-mgmt/integrations.go): when AUDIT_EVIDENCE_URL is set,
records are sealed to the core audit-evidence service
(POST /v1/evidence, content-addressed, MinIO WORM backend) and the WORM
receipt URI is kept. Otherwise a local append-only hash-chained JSONL log
under DATA_DIR is used (dev fallback, same idea as case-mgmt LocalWORM).

Forensic fields per record: actor, timestamp, str_id, tenant_id,
old_status, new_status, str_hash (SHA-256 of the canonical STR payload),
plus a hash chain (prev_chain -> chain) so tampering is detectable.
"""
from __future__ import annotations

import base64
import hashlib
import json
import os
import threading
from datetime import datetime, timezone

import httpx


def _canonical(record: dict) -> bytes:
    return json.dumps(record, sort_keys=True, separators=(",", ":")).encode()


class AuditTrail:
    def record(self, *, actor: str, str_id: str, tenant_id: str,
               old_status: str, new_status: str, str_hash: str,
               detail: str = "") -> dict:
        raise NotImplementedError


class LocalAuditTrail(AuditTrail):
    """Dev fallback: append-only hash-chained JSONL (WORM-style)."""

    source = "local"

    def __init__(self, path: str):
        self._path = path
        self._lock = threading.Lock()
        os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
        self._chain = "0" * 64
        if os.path.exists(path):
            with open(path, "rb") as fh:
                for line in fh:
                    if line.strip():
                        self._chain = json.loads(line)["chain"]

    def record(self, *, actor, str_id, tenant_id, old_status, new_status,
               str_hash, detail=""):
        rec = {
            "actor": actor,
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "str_id": str_id, "tenant_id": tenant_id,
            "old_status": old_status, "new_status": new_status,
            "str_hash": str_hash, "detail": detail,
            "prev_chain": self._chain,
        }
        rec["sha256"] = hashlib.sha256(_canonical(rec)).hexdigest()
        rec["chain"] = hashlib.sha256(
            (rec["prev_chain"] + rec["sha256"]).encode()).hexdigest()
        with self._lock:
            with open(self._path, "a", encoding="utf-8") as fh:
                fh.write(json.dumps(rec) + "\n")
            self._chain = rec["chain"]
        rec["worm_uri"] = f"worm://local/{rec['sha256']}"
        rec["source"] = self.source
        return rec


class HTTPAuditTrail(AuditTrail):
    """REAL: seal records to the core audit-evidence service (WORM)."""

    source = "core"

    def __init__(self, base_url: str, timeout: float = 3.0):
        self.base = base_url.rstrip("/")
        self._timeout = timeout

    def record(self, *, actor, str_id, tenant_id, old_status, new_status,
               str_hash, detail=""):
        rec = {
            "actor": actor,
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "str_id": str_id, "tenant_id": tenant_id,
            "old_status": old_status, "new_status": new_status,
            "str_hash": str_hash, "detail": detail,
        }
        raw = _canonical(rec)
        digest = hashlib.sha256(raw).hexdigest()
        body = {
            "kind": "str-filing-transition",
            "sha256": digest,
            "payload_b64": base64.b64encode(raw).decode(),
            "meta": {"str_id": str_id, "tenant_id": tenant_id,
                     "old_status": old_status, "new_status": new_status},
        }
        resp = httpx.post(self.base + "/v1/evidence", json=body,
                          timeout=self._timeout)
        resp.raise_for_status()
        out = resp.json() if resp.content else {}
        rec["sha256"] = digest
        rec["worm_uri"] = out.get("worm_uri") or f"worm://core/{out.get('id', digest)}"
        rec["source"] = self.source
        return rec


def audit_from_env(data_dir: str) -> AuditTrail:
    base = os.environ.get("AUDIT_EVIDENCE_URL", "")
    if base:
        return HTTPAuditTrail(base)
    return LocalAuditTrail(os.path.join(data_dir, "str_audit.jsonl"))
