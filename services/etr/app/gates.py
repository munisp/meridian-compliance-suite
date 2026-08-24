"""Server-side reg-watch gate resolution for the ETR service (audit fix B2-#10).

The QDMTT computation path (rp-etr-nta etr.nta.qdmtt) must activate only when
the core reg-watch `qdmtt_upgrade` gate is armed by a board-authorized flip.
Previously the API trusted a client-supplied `qdmtt_upgrade` request field,
letting any caller pick the cheaper top-up path. The server now resolves the
gate itself:

  1. reg-watch API (REG_WATCH_URL, GET /v1/gates, optional REG_WATCH_TOKEN
     bearer) — authoritative source.
  2. Local gate file fallback (ETR_GATE_FILE or DATA_DIR/gates.json,
     {"qdmtt_upgrade": true|false}) when reg-watch is unreachable.
  3. Fail-closed default: gate treated as disarmed (QDMTT NOT applied) when
     neither source is available — the safe path is the IIR residual top-up.

The client request field is ignored entirely.
"""
from __future__ import annotations

import json
import os

import httpx

GATE_ID = "qdmtt_upgrade"


def _gate_file() -> str:
    return os.environ.get("ETR_GATE_FILE") or os.path.join(
        os.environ.get("DATA_DIR", "./data"), "gates.json")


def qdmtt_upgrade_armed() -> tuple[bool, str]:
    """Return (armed, source). Fail-closed: (False, 'default') when unresolved."""
    reg_watch = os.environ.get("REG_WATCH_URL", "")
    if reg_watch:
        try:
            headers = {}
            token = os.environ.get("REG_WATCH_TOKEN", "")
            if token:
                headers["Authorization"] = f"Bearer {token}"
            resp = httpx.get(reg_watch.rstrip("/") + "/v1/gates",
                             headers=headers, timeout=1.5)
            if resp.status_code == 200:
                for gate in resp.json().get("gates", []):
                    if gate.get("id") == GATE_ID:
                        return gate.get("state") == "armed", "reg-watch"
        except Exception:
            pass  # fall through to local file / default
    try:
        with open(_gate_file(), "rb") as fh:
            local = json.load(fh)
        if GATE_ID in local:
            return bool(local[GATE_ID]), "local-file"
    except Exception:
        pass
    return False, "default"
