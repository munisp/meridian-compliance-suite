"""REST client for the Meridian NRS e-invoicing service.

Endpoints (services/einvoicing/nrs_handlers.go):
    POST  /v1/invoices/nrs    ingest NRS payload -> 8-step lifecycle (201/200)
    PATCH /v1/invoices/{irn}  payment_status / reference update (only)
    POST  /v1/webhooks        register stakeholder webhook endpoint
    GET   /v1/webhooks        list endpoints + delivery history

Auth is a Bearer token (API key) header; the Go service resolves tenant
claims from it. Stdlib urllib only — no third-party deps. A ``transport``
callable may be injected for tests: transport(method, url, body_bytes,
headers) -> (status_code, response_dict).
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request


class MeridianAPIError(RuntimeError):
    def __init__(self, message, status=None, problem=None):
        self.status = status
        self.problem = problem or {}
        super().__init__(message)


def _urllib_transport(method, url, body, headers, timeout):
    req = urllib.request.Request(url, data=body, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        try:
            return exc.code, json.loads(raw)
        except ValueError:
            return exc.code, {"title": raw.decode("utf-8", "replace")[:500]}
    except urllib.error.URLError as exc:
        raise MeridianAPIError("connection to Meridian service failed: %s" % exc)


class MeridianClient:
    def __init__(self, base_url, api_key, service_id, business_id,
                 timeout=30, transport=None):
        self.base_url = (base_url or "").rstrip("/")
        self.api_key = api_key or ""
        self.service_id = service_id or ""
        self.business_id = business_id or ""
        self.timeout = timeout
        self._transport = transport

    def _request(self, method, path, payload=None, extra_headers=None):
        if not self.base_url:
            raise MeridianAPIError("Meridian base URL not configured")
        body = json.dumps(payload).encode("utf-8") if payload is not None else None
        headers = {
            "Content-Type": "application/json",
            "Accept": "application/json",
        }
        if self.api_key:
            headers["Authorization"] = "Bearer %s" % self.api_key
        if extra_headers:
            headers.update(extra_headers)
        url = self.base_url + path
        if self._transport is not None:
            status, data = self._transport(method, url, body, headers)
        else:
            status, data = _urllib_transport(method, url, body, headers, self.timeout)
        if status >= 400:
            title = data.get("title") or "Meridian API error"
            detail = data.get("detail") or ""
            errors = data.get("errors") or []
            msg = "%s %s -> HTTP %s: %s %s" % (method, path, status, title, detail)
            if errors:
                msg += " | " + "; ".join(
                    "%s: %s (%s)" % (e.get("field"), e.get("message"), e.get("code"))
                    for e in errors
                )
            raise MeridianAPIError(msg.strip(), status=status, problem=data)
        return data

    def submit_invoice(self, nrs_payload, idempotency_key=None):
        """POST /v1/invoices/nrs. Returns the lifecycle response dict
        (irn, status, steps, crypto_stamp, qr, invoice, payment_status)."""
        headers = {}
        if idempotency_key:
            headers["Idempotency-Key"] = idempotency_key
        return self._request("POST", "/v1/invoices/nrs", nrs_payload, headers)

    def update_payment_status(self, irn, payment_status, payment_reference=None):
        """PATCH /v1/invoices/{irn} — payment_status is the only mutable
        field after signage (step 6 locks core fields)."""
        payload = {"payment_status": payment_status}
        if payment_reference:
            payload["payment_reference"] = payment_reference
        return self._request("PATCH", "/v1/invoices/%s" % irn, payload)

    def register_webhook(self, url, secret):
        """POST /v1/webhooks — register this Odoo instance as a stakeholder
        endpoint for the business (HMAC-SHA256 signed deliveries)."""
        return self._request(
            "POST", "/v1/webhooks",
            {"business_id": self.business_id, "url": url, "secret": secret},
        )

    def list_webhooks(self):
        return self._request("GET", "/v1/webhooks?business_id=%s" % self.business_id)
