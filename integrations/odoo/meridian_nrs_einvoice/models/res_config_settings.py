# -*- coding: utf-8 -*-
"""Settings: Meridian NRS connection parameters.

Stored via ir.config_parameter (company-agnostic; set per database):
    meridian_nrs.base_url       e.g. https://einvoicing.meridian.example
    meridian_nrs.api_key        Bearer token issued to this integrator
    meridian_nrs.service_id     NRS-assigned 8-char service id (e.g. 94ND90NR)
    meridian_nrs.business_id    Meridian business id (webhook routing key)
    meridian_nrs.webhook_secret >= 16-byte secret for HMAC delivery verify
    meridian_nrs.auto_submit    '1' to submit automatically on invoice post
"""

from odoo import fields, models

PARAM_KEYS = (
    "base_url",
    "api_key",
    "service_id",
    "business_id",
    "webhook_secret",
    "auto_submit",
)


class ResConfigSettings(models.TransientModel):
    _inherit = "res.config.settings"

    meridian_nrs_base_url = fields.Char(
        string="Meridian Base URL",
        config_parameter="meridian_nrs.base_url",
    )
    meridian_nrs_api_key = fields.Char(
        string="API Key",
        config_parameter="meridian_nrs.api_key",
    )
    meridian_nrs_service_id = fields.Char(
        string="NRS Service ID",
        config_parameter="meridian_nrs.service_id",
        help="8-character alphanumeric id assigned by NRS at onboarding.",
    )
    meridian_nrs_business_id = fields.Char(
        string="Meridian Business ID",
        config_parameter="meridian_nrs.business_id",
    )
    meridian_nrs_webhook_secret = fields.Char(
        string="Webhook Secret",
        config_parameter="meridian_nrs.webhook_secret",
        help="Shared secret (>= 16 chars) used to verify X-Meridian-Signature "
             "HMAC-SHA256 on inbound lifecycle webhooks.",
    )
    meridian_nrs_auto_submit = fields.Boolean(
        string="Submit on Invoice Post",
        config_parameter="meridian_nrs.auto_submit",
        default=True,
        help="When enabled, customer invoices/credit notes are queued for "
             "NRS clearance automatically when posted.",
    )
