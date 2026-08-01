# -*- coding: utf-8 -*-
"""account.move extension: NRS e-invoice clearance via the Meridian
einvoicing service.

Flow:
  1. Post a customer invoice / credit note -> (auto or manual) queued and
     submitted as an NRS UBL-shaped JSON payload.
  2. The Meridian service runs the 8-step lifecycle; the coarse status is
     mirrored on nrs_clearance_status; IRN + QR payload are stored.
  3. Lifecycle webhooks (HMAC-verified, see controllers/webhook.py) and the
     cron poller advance the status; failures land in nrs.submission.log.
  4. When the invoice is paid, payment_status is PATCHed to the service by
     IRN (the only mutable field after signage).

This file imports `odoo` and therefore requires an Odoo host (17/18). The
mapping/HMAC/client logic lives in the pure-Python `meridian_odoo_client`
package, which is import-guarded so the addon can be inspected without it.
"""

import json
import logging
from datetime import timedelta

from odoo import _, api, fields, models
from odoo.exceptions import UserError

_logger = logging.getLogger(__name__)

try:
    from meridian_odoo_client import (
        MeridianAPIError,
        MeridianClient,
        NRSPayloadError,
        build_nrs_invoice,
        map_service_status,
    )
    _CLIENT_AVAILABLE = True
    _CLIENT_IMPORT_ERROR = None
except ImportError as exc:  # pragma: no cover - depends on host PYTHONPATH
    _CLIENT_AVAILABLE = False
    _CLIENT_IMPORT_ERROR = exc
    _logger.warning(
        "meridian_odoo_client not importable (%s). NRS submission disabled; "
        "install the package on the Odoo host PYTHONPATH (see docs/ERP-ODOO.md).",
        exc,
    )


class AccountMove(models.Model):
    _inherit = "account.move"

    nrs_irn = fields.Char(
        string="NRS IRN", copy=False, readonly=True, tracking=True,
        help="Invoice Reference Number: <InvoiceNumber>-<ServiceID>-<YYYYMMDD>",
    )
    nrs_clearance_status = fields.Selection(
        selection=[
            ("draft", "Not Submitted"),
            ("submitted", "Submitted"),
            ("signed", "Signed"),
            ("transmitted", "Transmitted"),
            ("confirmed", "Cleared (Confirmed)"),
            ("failed", "Failed"),
        ],
        string="NRS Clearance", default="draft", copy=False, readonly=True,
        tracking=True, index=True,
    )
    nrs_qr_payload = fields.Text(string="NRS QR Payload", copy=False, readonly=True)
    nrs_qr_svg = fields.Text(string="NRS QR (SVG)", copy=False, readonly=True)
    nrs_last_error = fields.Text(string="NRS Last Error", copy=False, readonly=True)
    nrs_payment_status = fields.Selection(
        selection=[("PENDING", "Pending"), ("PAID", "Paid"), ("REJECTED", "Rejected")],
        string="NRS Payment Status", default="PENDING", copy=False, readonly=True,
    )
    nrs_log_ids = fields.One2many("nrs.submission.log", "move_id", string="NRS Log")

    # ------------------------------------------------------------------
    # configuration / client
    # ------------------------------------------------------------------

    def _nrs_params(self):
        get = self.env["ir.config_parameter"].sudo().get_param
        return {
            "base_url": get("meridian_nrs.base_url", ""),
            "api_key": get("meridian_nrs.api_key", ""),
            "service_id": get("meridian_nrs.service_id", ""),
            "business_id": get("meridian_nrs.business_id", ""),
            "webhook_secret": get("meridian_nrs.webhook_secret", ""),
            "auto_submit": get("meridian_nrs.auto_submit", "False")
            in ("1", "true", "True"),
        }

    def _nrs_client(self):
        if not _CLIENT_AVAILABLE:
            raise UserError(
                _("meridian_odoo_client is not installed on this Odoo host: %s")
                % _CLIENT_IMPORT_ERROR
            )
        p = self._nrs_params()
        if not p["base_url"] or not p["service_id"] or not p["business_id"]:
            raise UserError(
                _("Meridian NRS is not configured. Set base URL, service id "
                  "and business id under Settings > Meridian NRS e-Invoicing.")
            )
        return MeridianClient(
            p["base_url"], p["api_key"], p["service_id"], p["business_id"],
        )

    def _nrs_is_in_scope(self):
        return self.is_sale_document(include_receipts=False) and self.move_type in (
            "out_invoice",
            "out_refund",
        )

    # ------------------------------------------------------------------
    # payload construction (neutral dict -> meridian_odoo_client)
    # ------------------------------------------------------------------

    def _nrs_party_dict(self, partner):
        state = partner.state_id
        return {
            "name": partner.name or "",
            # vat carries the Nigerian TIN on res.partner
            "tin": (partner.vat or "").replace(" ", ""),
            "email": partner.email or "",
            "phone": partner.phone or "",
            "street": partner.street or "",
            "city": partner.city or "",
            "zip": partner.zip or "",
            "state": state.code if state else "",
            "country": partner.country_id.code if partner.country_id else "NG",
        }

    def _nrs_export_dict(self):
        """Build the neutral invoice mapping consumed by
        meridian_odoo_client.build_nrs_invoice. See docs/ERP-ODOO.md for the
        field mapping table."""
        self.ensure_one()
        company_partner = self.company_id.partner_id
        lines = []
        for line in self.invoice_line_ids.filtered(lambda l: l.display_type not in (
                "line_section", "line_note")):
            tax = line.tax_ids[:1]
            lines.append({
                "name": line.name or (line.product_id.display_name if line.product_id else ""),
                "description": line.product_id.description_sale if line.product_id else "",
                "quantity": line.quantity,
                "price_unit": line.price_unit,
                "discount_rate": line.discount or 0.0,
                "line_extension_amount": line.price_subtotal,
                "tax_category": (tax.nrs_tax_category if tax else "STANDARD_VAT"),
                "hsn_code": getattr(line.product_id, "hs_code", "") or "",
                "sellers_item_identification": line.product_id.default_code
                if line.product_id else "",
            })
        return {
            "invoice_number": (self.name or "").replace("/", ""),
            "issue_date": fields.Date.to_string(
                self.invoice_date or fields.Date.context_today(self)),
            "due_date": fields.Date.to_string(self.invoice_date_due)
            if self.invoice_date_due else "",
            "move_type": self.move_type,
            "currency": self.currency_id.name or "NGN",
            "supplier": self._nrs_party_dict(company_partner),
            "customer": self._nrs_party_dict(self.partner_id),
            "lines": lines,
            "note": self.narration or "",
            "buyer_reference": (self.name or "").replace("/", ""),
            "order_reference": self.ref or "",
            "payment_means_code": "30",  # credit transfer
            "irn": self.nrs_irn or "",
        }

    # ------------------------------------------------------------------
    # submission
    # ------------------------------------------------------------------

    def action_post(self):
        res = super().action_post()
        params = self._nrs_params()
        if params["auto_submit"]:
            for move in self.filtered(self._nrs_is_in_scope):
                try:
                    move.action_nrs_submit()
                except Exception as exc:  # never block posting on NRS failure
                    _logger.exception("NRS auto-submit failed for %s", move.name)
                    move._nrs_record_error(exc)
        return res

    def action_nrs_submit(self):
        """Button / cron entry point: build payload, POST to Meridian,
        store IRN + QR, mirror lifecycle status. Idempotent via the IRN."""
        for move in self:
            if not move._nrs_is_in_scope():
                raise UserError(_("Only customer invoices and credit notes "
                                  "are submitted to NRS."))
            if move.state != "posted":
                raise UserError(_("Invoice %s must be posted before NRS "
                                  "submission.") % move.name)
            if move.nrs_clearance_status == "confirmed":
                continue
            params = move._nrs_params()
            try:
                payload = build_nrs_invoice(
                    move._nrs_export_dict(), params["service_id"],
                    params["business_id"])
            except NRSPayloadError as exc:
                move._nrs_record_error(exc)
                raise UserError(
                    _("NRS payload mapping failed for %s:\n%s")
                    % (move.name, exc)) from exc
            log = move._nrs_log("queued", payload=json.dumps(payload))
            try:
                resp = move._nrs_client().submit_invoice(
                    payload, idempotency_key="odoo-%s-%s" % (move._name, move.id))
            except (MeridianAPIError, UserError) as exc:
                move._nrs_record_error(exc, log=log)
                if not self.env.context.get("nrs_cron"):
                    raise UserError(
                        _("NRS submission failed for %s:\n%s") % (move.name, exc)
                    ) from exc
                continue
            move._nrs_apply_response(resp, log=log)
        return True

    def _nrs_apply_response(self, resp, log=None):
        self.ensure_one()
        qr = resp.get("qr") or {}
        vals = {
            "nrs_irn": resp.get("irn") or self.nrs_irn,
            "nrs_clearance_status": map_service_status(resp.get("status", "")),
            "nrs_payment_status": resp.get("payment_status") or "PENDING",
            "nrs_qr_payload": qr.get("payload", ""),
            "nrs_qr_svg": qr.get("qr_svg", ""),
            "nrs_last_error": False,
        }
        if resp.get("error"):
            vals["nrs_clearance_status"] = "failed"
            vals["nrs_last_error"] = resp["error"]
        self.write(vals)
        if log:
            log.write({
                "state": "sent" if not resp.get("error") else "error",
                "irn": vals["nrs_irn"],
                "attempts": log.attempts + 1,
                "last_error": resp.get("error") or False,
            })
        if resp.get("error"):
            self._nrs_schedule_retry(log)

    def _nrs_record_error(self, exc, log=None):
        self.ensure_one()
        msg = str(exc)
        self.write({"nrs_clearance_status": "failed", "nrs_last_error": msg})
        log = log or self._nrs_log("error")
        log.write({"state": "error", "attempts": log.attempts + 1,
                   "last_error": msg})
        self._nrs_schedule_retry(log)

    def _nrs_schedule_retry(self, log):
        if log and log.attempts < log.max_attempts:
            log.write({
                "state": "queued",
                # linear backoff, mirroring the service's webhook retry
                "next_retry": fields.Datetime.now() + timedelta(
                    minutes=15 * (log.attempts + 1)),
            })

    def _nrs_log(self, state, payload=""):
        self.ensure_one()
        return self.env["nrs.submission.log"].sudo().create({
            "move_id": self.id,
            "irn": self.nrs_irn or "",
            "payload": payload,
            "state": state,
        })

    # ------------------------------------------------------------------
    # webhook / poller updates
    # ------------------------------------------------------------------

    def _nrs_handle_webhook_event(self, event, data):
        """Called by the webhook controller after HMAC verification.
        Out-of-order deliveries are tolerated: status only moves forward."""
        self.ensure_one()
        order = ["draft", "submitted", "signed", "transmitted", "confirmed"]
        target = map_service_status(data.get("status", ""))
        if "failed" in (data.get("status") or "") or event.endswith(".failed.v1"):
            target = "failed"
        vals = {}
        if data.get("irn") and not self.nrs_irn:
            vals["nrs_irn"] = data["irn"]
        if target == "failed":
            vals["nrs_clearance_status"] = "failed"
        elif target in order and self.nrs_clearance_status in order and \
                order.index(target) > order.index(self.nrs_clearance_status):
            vals["nrs_clearance_status"] = target
        if data.get("payment_status"):
            vals["nrs_payment_status"] = data["payment_status"]
        if vals:
            self.write(vals)
        self._nrs_log("sent").write({"event": event,
                                     "payload": json.dumps(data)})
        return True

    @api.model
    def _cron_nrs_process_queue(self, log_limit=50):
        """Retry queued error-queue entries whose backoff has elapsed."""
        logs = self.env["nrs.submission.log"].sudo().search([
            ("state", "=", "queued"),
            "|", ("next_retry", "=", False),
            ("next_retry", "<=", fields.Datetime.now()),
        ], limit=log_limit)
        moves = logs.mapped("move_id")
        if moves:
            moves.with_context(nrs_cron=True).action_nrs_submit()

    @api.model
    def _cron_nrs_sync_payment_status(self, limit=100):
        """Push payment_status to the service for paid invoices whose NRS
        payment status is still PENDING (PATCH /v1/invoices/{irn})."""
        moves = self.search([
            ("nrs_irn", "!=", False),
            ("nrs_clearance_status", "in", ("signed", "transmitted", "confirmed")),
            ("nrs_payment_status", "=", "PENDING"),
            ("payment_state", "in", ("paid", "in_payment")),
        ], limit=limit)
        for move in moves:
            try:
                move._nrs_client().update_payment_status(
                    move.nrs_irn, "PAID",
                    payment_reference="odoo-move-%s" % move.id)
                move.write({"nrs_payment_status": "PAID"})
            except Exception as exc:
                _logger.warning("NRS payment PATCH failed for %s: %s",
                                move.nrs_irn, exc)
