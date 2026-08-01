# Odoo 17/18 compatibility: Odoo requires the manifest "version" to start
# with the running series ("17.0." on Odoo 17, "18.0." on Odoo 18) and
# refuses to install otherwise. The addon code itself is series-tolerant
# (no version-gated APIs; see docs/ERP-ODOO.md "Odoo 17/18 compatibility").
# When deploying on Odoo 18, change ONLY the prefix to "18.0.1.1.0" — no
# other manifest key changes are required. tools/test_addon_structure.py
# asserts the prefix is 17.0 or 18.0.
{
    "name": "Meridian NRS e-Invoicing",
    "version": "17.0.1.1.0",
    "category": "Accounting/Accounting",
    "summary": "Nigeria NRS e-invoice clearance for customer invoices via the "
               "Meridian einvoicing service (IRN, 8-step lifecycle, QR, HMAC "
               "webhooks).",
    "description": """Meridian NRS e-Invoicing
=========================
Submits posted customer invoices and credit notes to the Meridian NRS
e-invoicing service as NRS UBL-shaped JSON (IRN = InvoiceNumber-ServiceID-
YYYYMMDD, integer-kobo round-half-up totals, VAT 7.5% / zero-rated medical
& tuition categories), tracks the 8-step clearance lifecycle on the invoice,
receives HMAC-SHA256-signed lifecycle webhooks, pushes payment status by IRN,
and retries failures from an error queue.

Requires the pure-Python package ``meridian_odoo_client`` on the Odoo host
PYTHONPATH (shipped alongside this addon in integrations/odoo/). Supports
Odoo 17 and Odoo 18 (see docs/ERP-ODOO.md, "Odoo 17/18 compatibility":
manifest version prefix, cron numbercall, settings-view anchors).""",
    "author": "Meridian Compliance Suite",
    "license": "LGPL-3",
    "depends": ["account"],
    "data": [
        "security/ir.model.access.csv",
        "data/ir_cron.xml",
        "views/account_move_views.xml",
        "views/nrs_submission_log_views.xml",
        "views/res_config_settings_views.xml",
    ],
    "installable": True,
    "application": False,
    "auto_install": False,
}
