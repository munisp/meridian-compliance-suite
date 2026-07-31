{
    "name": "Meridian NRS e-Invoicing",
    "version": "17.0.1.0.0",
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
PYTHONPATH (shipped alongside this addon in integrations/odoo/). Tested
against Odoo 17; Odoo 18 compatible API. See docs/ERP-ODOO.md.""",
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
