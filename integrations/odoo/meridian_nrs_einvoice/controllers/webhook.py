# -*- coding: utf-8 -*-
"""Inbound Meridian lifecycle webhook endpoint.

The Meridian einvoicing service POSTs lifecycle events (step 7 transmit /
step 8 confirm / failures) to registered stakeholder endpoints with
``X-Meridian-Signature: <hex HMAC-SHA256(secret, raw body)>``. We verify the
signature against ir.config_parameter ``meridian_nrs.webhook_secret`` before
touching anything, then resolve the invoice by IRN and advance its
clearance status.

Route: POST /meridian_nrs/webhook  (auth='none', csrf=False)
"""

import json
import logging

from odoo import http
from odoo.http import request

_logger = logging.getLogger(__name__)

try:
    from meridian_odoo_client import verify_webhook_signature
    _CLIENT_AVAILABLE = True
except ImportError:  # pragma: no cover
    _CLIENT_AVAILABLE = False
    _logger.warning("meridian_odoo_client missing; NRS webhook endpoint will "
                    "reject all deliveries (503).")


class MeridianNRSWebhook(http.Controller):

    @http.route("/meridian_nrs/webhook", type="http", auth="none",
                methods=["POST"], csrf=False)
    def nrs_webhook(self, **kw):
        if not _CLIENT_AVAILABLE:
            return request.make_response(
                json.dumps({"error": "client package not installed"}),
                status=503, headers=[("Content-Type", "application/json")])
        body = request.httprequest.get_data() or b""
        signature = request.httprequest.headers.get("X-Meridian-Signature", "")
        event = request.httprequest.headers.get("X-Meridian-Event", "")
        secret = request.env["ir.config_parameter"].sudo().get_param(
            "meridian_nrs.webhook_secret", "")
        if not secret:
            _logger.error("NRS webhook received but no webhook_secret configured")
            return self._json({"error": "webhook not configured"}, 503)
        if not verify_webhook_signature(secret, body, signature):
            _logger.warning("NRS webhook signature rejected (event=%s)", event)
            return self._json({"error": "invalid signature"}, 401)
        try:
            envelope = json.loads(body)
        except ValueError:
            return self._json({"error": "invalid json"}, 400)
        data = envelope.get("data") or {}
        irn = data.get("irn") or ""
        if not irn:
            return self._json({"error": "irn missing"}, 400)
        move = request.env["account.move"].sudo().search(
            [("nrs_irn", "=", irn)], limit=1)
        if not move:
            # Unknown IRN: 200 so the service stops retrying; log for audit.
            _logger.info("NRS webhook for unknown IRN %s (event=%s)", irn, event)
            return self._json({"status": "ignored", "reason": "unknown irn"}, 200)
        move._nrs_handle_webhook_event(event or envelope.get("event", ""), data)
        return self._json({"status": "ok", "irn": irn}, 200)

    @staticmethod
    def _json(payload, status):
        return request.make_response(
            json.dumps(payload), status=status,
            headers=[("Content-Type", "application/json")])
