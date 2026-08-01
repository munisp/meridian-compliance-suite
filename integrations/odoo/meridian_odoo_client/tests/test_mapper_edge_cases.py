"""Mapper edge cases: rounding boundary sweep, missing partner TIN,
>99-line invoices, credit-note sign handling, currency rejection.

Complements test_schema.py (round-trip happy paths) with the boundary and
failure modes flagged in review.
"""

import unittest

from meridian_odoo_client.schema import (
    NRSPayloadError,
    build_nrs_invoice,
    ngn_to_kobo,
    round_bps_half_up,
)

SERVICE_ID = "94ND90NR"
BUSINESS_ID = "biz-odoo-001"

SUPPLIER = {"name": "Lekki Medical Supplies Ltd", "tin": "12345678-0001",
            "country": "NG"}
CUSTOMER = {"name": "Ikeja General Hospital", "tin": "87654321-0001",
            "country": "NG"}


def invoice(**over):
    inv = {
        "invoice_number": "INV20260099",
        "issue_date": "2026-03-05",
        "move_type": "out_invoice",
        "currency": "NGN",
        "supplier": dict(SUPPLIER),
        "customer": dict(CUSTOMER),
        "lines": [
            {"name": "Surgical gloves (box)", "quantity": 1,
             "price_unit": 1000.00, "line_extension_amount": 1000.00,
             "tax_category": "STANDARD_VAT"},
        ],
    }
    inv.update(over)
    return inv


class TestRoundingBoundarySweep(unittest.TestCase):
    """x.xx5 half-up boundaries must round UP in decimal space, never
    fall victim to binary float representation (e.g. 2.675 -> 268 kobo,
    not 267)."""

    CASES = [
        (0.005, 1),      # half kobo up
        (0.015, 2),
        (0.575, 58),
        (1.005, 101),
        (2.675, 268),    # float repr 2.6749999... must still round up
        (10.555, 1056),
        (99.995, 10000),
        (0.004, 0),      # just below half stays down
        (0.994, 99),
        (1234.565, 123457),
    ]

    def test_ngn_to_kobo_sweep(self):
        for amount, expected_kobo in self.CASES:
            self.assertEqual(ngn_to_kobo(amount), expected_kobo,
                             "ngn_to_kobo(%r)" % amount)

    def test_ngn_to_kobo_sweep_via_decimal_strings(self):
        # the same boundaries expressed as strings (JSON-safe path)
        for amount, expected_kobo in (
                ("0.005", 1), ("2.675", 268), ("99.995", 10000)):
            self.assertEqual(ngn_to_kobo(amount), expected_kobo)

    def test_round_bps_half_up_sweep(self):
        # amount_kobo * 750 / 10000: n = amount*750; boundary at n%10000==5000
        for amount_kobo, expected in (
                (10000, 750),     # exact 7.5%
                (67, 5),          # 67*750=50250 -> 5.025 -> 5
                (73, 5),          # 73*750=54750 -> 5.475 -> 5
                (7, 1),           # 7*750=5250 -> 0.525 -> 1 (half-up)
                (6, 0),           # 6*750=4500 -> 0.45  -> 0
                (1, 0),           # 1*750=750   -> 0.075 -> 0
                (100000, 7500),
        ):
            self.assertEqual(round_bps_half_up(amount_kobo, 750), expected,
                             "round_bps_half_up(%d, 750)" % amount_kobo)

    def test_round_bps_half_up_negative_mirror(self):
        # sign-symmetric (used only defensively; mapper rejects negatives)
        self.assertEqual(round_bps_half_up(-67, 750), -5)
        self.assertEqual(round_bps_half_up(-7, 750), -1)


class TestMissingPartnerTIN(unittest.TestCase):
    def test_missing_supplier_tin_clear_error(self):
        inv = invoice(supplier={"name": "No TIN Ltd", "country": "NG"})
        with self.assertRaises(NRSPayloadError) as ctx:
            build_nrs_invoice(inv, SERVICE_ID, BUSINESS_ID)
        self.assertIn("accounting_supplier_party.tin is required",
                      str(ctx.exception))

    def test_blank_supplier_tin_clear_error(self):
        inv = invoice(supplier={"name": "Blank TIN Ltd", "tin": "   ",
                                "country": "NG"})
        with self.assertRaises(NRSPayloadError) as ctx:
            build_nrs_invoice(inv, SERVICE_ID, BUSINESS_ID)
        self.assertIn("accounting_supplier_party.tin is required",
                      str(ctx.exception))

    def test_missing_supplier_name_also_reported_together(self):
        # all mapping errors are aggregated, not fail-fast on the first
        inv = invoice(supplier={"country": "NG"})
        with self.assertRaises(NRSPayloadError) as ctx:
            build_nrs_invoice(inv, SERVICE_ID, BUSINESS_ID)
        msg = str(ctx.exception)
        self.assertIn("accounting_supplier_party.party_name is required", msg)
        self.assertIn("accounting_supplier_party.tin is required", msg)

    def test_missing_customer_tin_allowed_b2c(self):
        inv = invoice(customer={"name": "Walk-in Customer", "country": "NG"})
        payload = build_nrs_invoice(inv, SERVICE_ID, BUSINESS_ID)
        self.assertEqual(
            payload["accounting_customer_party"]["party_name"],
            "Walk-in Customer")


class TestManyLinesPagination(unittest.TestCase):
    """Odoo's default one2many list pagination (80/record) must not
    truncate the export: _nrs_export_dict iterates invoice_line_ids
    directly, so >99 lines all land in one payload."""

    def test_150_lines_all_present_with_correct_totals(self):
        lines = []
        for i in range(150):
            lines.append({
                "name": "Item %03d" % i,
                "quantity": 1,
                "price_unit": 10.00,
                "line_extension_amount": 10.00,
                "tax_category": "STANDARD_VAT",
            })
        payload = build_nrs_invoice(invoice(lines=lines), SERVICE_ID,
                                    BUSINESS_ID)
        self.assertEqual(len(payload["invoice_line"]), 150)
        total = payload["legal_monetary_total"]
        self.assertEqual(total["tax_exclusive_amount"], 1500.00)
        # 7.5% of 150000 kobo = 11250 kobo = 112.50
        self.assertEqual(total["payable_amount"], 1612.50)
        sub = payload["tax_total"][0]["tax_subtotal"][0]
        self.assertEqual(sub["taxable_amount"], 1500.00)
        self.assertEqual(sub["tax_amount"], 112.50)

    def test_101_lines_mixed_categories(self):
        lines = [
            {"name": "Item %03d" % i, "quantity": 1, "price_unit": 5.00,
             "line_extension_amount": 5.00,
             "tax_category": "STANDARD_VAT" if i % 2 else "ZERO_VAT"}
            for i in range(101)
        ]
        payload = build_nrs_invoice(invoice(lines=lines), SERVICE_ID,
                                    BUSINESS_ID)
        self.assertEqual(len(payload["invoice_line"]), 101)
        subs = {s["tax_category"]["id"]: s
                for s in payload["tax_total"][0]["tax_subtotal"]}
        self.assertEqual(set(subs), {"STANDARD_VAT", "ZERO_VAT"})


class TestCreditNoteSignHandling(unittest.TestCase):
    def test_credit_note_type_381_positive_amounts(self):
        inv = invoice(move_type="out_refund", invoice_number="RINV20260001")
        payload = build_nrs_invoice(inv, SERVICE_ID, BUSINESS_ID)
        self.assertEqual(payload["invoice_type_code"], "381")
        self.assertEqual(payload["irn"], "RINV20260001-94ND90NR-20260305")
        # Odoo credit-note lines export positive price_subtotal; payload
        # must carry positive amounts
        line = payload["invoice_line"][0]
        self.assertGreater(line["line_extension_amount"], 0)
        self.assertGreater(line["price"]["price_amount"], 0)
        self.assertGreater(
            payload["legal_monetary_total"]["payable_amount"], 0)

    def test_negative_line_extension_rejected_with_clear_error(self):
        inv = invoice(lines=[
            {"name": "Bad sign", "quantity": 1, "price_unit": 100.00,
             "line_extension_amount": -100.00,
             "tax_category": "STANDARD_VAT"},
        ])
        with self.assertRaises(NRSPayloadError) as ctx:
            build_nrs_invoice(inv, SERVICE_ID, BUSINESS_ID)
        self.assertIn("line_extension_amount must be >= 0",
                      str(ctx.exception))

    def test_negative_price_rejected_with_clear_error(self):
        inv = invoice(lines=[
            {"name": "Bad price", "quantity": 1, "price_unit": -50.00,
             "line_extension_amount": 50.00,
             "tax_category": "STANDARD_VAT"},
        ])
        with self.assertRaises(NRSPayloadError) as ctx:
            build_nrs_invoice(inv, SERVICE_ID, BUSINESS_ID)
        self.assertIn("price.price_amount must be >= 0", str(ctx.exception))


class TestForeignCurrencyRejected(unittest.TestCase):
    def test_usd_rejected_clear_error(self):
        with self.assertRaises(NRSPayloadError) as ctx:
            build_nrs_invoice(invoice(currency="USD"), SERVICE_ID,
                              BUSINESS_ID)
        msg = str(ctx.exception)
        self.assertIn("USD", msg)
        self.assertIn("NGN", msg)

    def test_eur_lowercase_rejected(self):
        with self.assertRaises(NRSPayloadError):
            build_nrs_invoice(invoice(currency="eur"), SERVICE_ID,
                              BUSINESS_ID)

    def test_ngn_case_insensitive_accepted(self):
        payload = build_nrs_invoice(invoice(currency="ngn"), SERVICE_ID,
                                    BUSINESS_ID)
        self.assertEqual(payload["document_currency_code"], "NGN")


if __name__ == "__main__":
    unittest.main()
