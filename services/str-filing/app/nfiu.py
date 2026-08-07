"""NFIU submission adapters.

Adapter interface (`NFIUClient`) with two implementations:

- ``HTTPNFIUClient`` (transport=HTTP, REAL): posts the STR to the NFIU
  goAML/e-filing endpoint at NFIU_BASE_URL. This is the default and the ONLY
  adapter allowed when PROFILE=prod / AUTH_MODE=keycloak (fail-closed).
- ``SimNFIUClient`` (transport=SIM, SIMULATED): in-process fake used for
  dev/tests and the NFIU-outage runbook. It never leaves the process and is
  refused in prod profile. Tagged SIM per the platform REAL/SIM honesty
  convention.

Retryability contract: ``NFIUUnavailableError`` => retryable (backoff);
``NFIURejectedError`` => permanent (straight to DLQ, no retry).
"""
from __future__ import annotations

import os
import threading

import httpx


class NFIUError(Exception):
    """Base submission error."""


class NFIUUnavailableError(NFIUError):
    """Retryable: transport failure or NFIU 5xx / 429."""


class NFIURejectedError(NFIUError):
    """Permanent: NFIU 4xx — the report was refused; do not retry."""


class NFIUClient:
    transport = "abstract"

    def submit(self, *, str_id: str, tenant_id: str, report_type: str,
               payload: str, idempotency_key: str) -> str:
        """Submit one STR; returns the NFIU acknowledgement reference."""
        raise NotImplementedError


class HTTPNFIUClient(NFIUClient):
    transport = "HTTP"

    def __init__(self, base_url: str, timeout: float = 10.0,
                 api_key: str = "", client: httpx.Client | None = None):
        if not base_url:
            raise ValueError("NFIU_BASE_URL is required for the HTTP adapter")
        self.base = base_url.rstrip("/")
        self._client = client or httpx.Client(timeout=timeout)
        self._api_key = api_key

    def submit(self, *, str_id, tenant_id, report_type, payload,
               idempotency_key):
        headers = {"Content-Type": "application/json",
                   "X-Idempotency-Key": idempotency_key,
                   "X-Tenant-Id": tenant_id}
        if self._api_key:
            headers["Authorization"] = f"Bearer {self._api_key}"
        try:
            resp = self._client.post(f"{self.base}/v1/str", content=payload,
                                     headers=headers)
        except httpx.HTTPError as exc:
            raise NFIUUnavailableError(f"nfiu transport: {exc}") from exc
        if resp.status_code in (429,) or resp.status_code >= 500:
            raise NFIUUnavailableError(f"nfiu status {resp.status_code}")
        if resp.status_code >= 400:
            raise NFIURejectedError(
                f"nfiu status {resp.status_code}: {resp.text[:200]}")
        try:
            return resp.json().get("reference") or resp.json().get("id") or ""
        except ValueError:
            return ""


class SimNFIUClient(NFIUClient):
    """SIM adapter (SIMULATED transport — dev/test/runbook only)."""

    transport = "SIM"

    def __init__(self):
        self._lock = threading.Lock()
        self.available = True          # runbook: flip to simulate an outage
        self.fail_permanent = False    # simulate a 4xx rejection
        self.submissions: list[dict] = []

    def submit(self, *, str_id, tenant_id, report_type, payload,
               idempotency_key):
        with self._lock:
            if not self.available:
                raise NFIUUnavailableError("SIM nfiu status 503")
            if self.fail_permanent:
                raise NFIURejectedError("SIM nfiu status 422: rejected")
            self.submissions.append({
                "str_id": str_id, "tenant_id": tenant_id,
                "report_type": report_type, "payload": payload,
                "idempotency_key": idempotency_key,
            })
            return f"SIM-NFIU-REF-{len(self.submissions):06d}"


def adapter_from_env() -> NFIUClient:
    """Select adapter. Prod profile (PROFILE=prod or AUTH_MODE=keycloak)
    fails closed on anything but the real HTTP adapter."""
    prod = (os.environ.get("PROFILE", "").lower() == "prod"
            or os.environ.get("AUTH_MODE", "").lower() in ("keycloak", "prod"))
    kind = os.environ.get("STR_NFIU_ADAPTER", "http").lower()
    if kind == "sim":
        if prod:
            raise RuntimeError(
                "STR_NFIU_ADAPTER=sim refused in prod profile "
                "(SIM transport is dev/test only)")
        return SimNFIUClient()
    return HTTPNFIUClient(
        base_url=os.environ.get("NFIU_BASE_URL", ""),
        api_key=os.environ.get("NFIU_API_KEY", ""),
    )
