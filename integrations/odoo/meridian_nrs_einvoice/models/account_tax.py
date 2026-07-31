# -*- coding: utf-8 -*-
"""account.tax extension: NRS tax category mapping.

Per NTA 2025 s.187, medical services and tuition are ZERO-RATED (input VAT
recoverable) — configure those taxes with nrs_tax_category = ZERO_VAT. The
mapper (meridian_odoo_client.schema) groups lines by this category to build
the NRS tax_total subtotals.
"""

from odoo import fields, models


class AccountTax(models.Model):
    _inherit = "account.tax"

    nrs_tax_category = fields.Selection(
        selection=[
            ("STANDARD_VAT", "Standard VAT 7.5%"),
            ("ZERO_VAT", "Zero-rated (medical / tuition / exports)"),
            ("EXEMPT", "VAT exempt"),
            ("NON_VAT", "Outside scope of VAT"),
        ],
        string="NRS Tax Category",
        default="STANDARD_VAT",
        help="How this tax is reported to NRS e-invoicing. Zero-rated and "
             "exempt lines are reported at 0% under their own category.",
    )
