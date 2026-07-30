"""Event envelope helpers (SPEC 1.1)."""

from __future__ import annotations

import os
import secrets
import threading
import time
from datetime import datetime, timezone

_CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
_lock = threading.Lock()
_last_ms = 0


def ulid() -> str:
    """26-char Crockford-base32 ULID (time-ordered)."""
    global _last_ms
    with _lock:
        now = int(time.time() * 1000)
        if now < _last_ms:
            now = _last_ms
        _last_ms = now
    ms = now
    hi = []
    for _ in range(10):
        hi.append(_CROCKFORD[ms & 31])
        ms >>= 5
    rand_bits = int.from_bytes(secrets.token_bytes(10), "big")
    lo = []
    for _ in range(16):
        lo.append(_CROCKFORD[rand_bits & 31])
        rand_bits >>= 5
    return "".join(reversed(hi)) + "".join(reversed(lo))


def new_envelope(event_type: str, source: str, data: dict,
                 tenant_id: str = "", rule_pack_version: str = "") -> dict:
    """Build a SPEC 1.1 envelope."""
    return {
        "id": ulid(),
        "type": event_type,
        "source": source,
        "time": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "tenant_id": tenant_id,
        "trace_id": secrets.token_hex(16),
        "rule_pack_version": rule_pack_version,
        "data": data,
    }


class InprocBus:
    """Dev in-process bus (EVENT_BUS=inproc default)."""

    def __init__(self) -> None:
        self._messages: dict[str, list[dict]] = {}

    def publish(self, topic: str, env: dict) -> None:
        self._messages.setdefault(topic, []).append(env)

    def messages(self, topic: str) -> list[dict]:
        return list(self._messages.get(topic, []))


def event_bus_kind() -> str:
    return os.environ.get("EVENT_BUS", "inproc")
