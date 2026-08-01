"""Round-trip: sample Odoo invoice dict -> NRS payload -> validate against
the expectations of services/einvoicing/nrs_schema.go (IRN format, integer
kobo round-half-up, VAT 7.5%, zero-rated medical/tuition, credit notes).
"""

import unittest

from meridian_odoo_client.schema import (
    NRSPayloadError,
    build_irn,
    build_nrs_invoice,
    date_stamp,
    kobo_to_ngn,
    ngn_to_kobo,
    parse_irn,
    round_bps_half_up,
    valid_irn,
    valid_service_id,
)

SERVICE_ID = "94ND90NR"
BUSINESS_ID = "biz-odoo-001"

SUPPLIER = {
    "name": "Lekki Medical Supplies Ltd",
    "tin": "12345678-0001",
    "email": "accounts@lekki-med.ng",
    "phone": "+2348012345678",
    "street": "14 Admiralty Way",
    "city": "Lekki",
    "state": "NG-LA",
    "country": "NG",
}
CUSTOMER = {
    "name": "Ikeja General Hospital",
    "tin": "87654321-0001",
    "email": "procurement@ikejagen.ng",
    "city": "Ikeja",
    "state": "NG-LA",
    "country": "NG",
}


def sample_invoice(**over):
    inv = {
        "invoice_number": "INV20260001",
        "issue_date": "2026-01-27",
        "due_date": "2026-02-26",
        "move_type": "out_invoice",
        "currency": "NGN",
        "supplier": SUPPLIER,
        "customer": CUSTOMER,
        "payment_means_code": "30",
        "lines": [
            {   # standard VAT 7.5%
                "name": "Surgical gloves (box)",
                "quantity": 2,
                "price_unit": 5000.00,
                "line_extension_amount": 10000.00,
                "tax_category": "STANDARD_VAT",
            },
            {   # zero-rated: medical services (NTA 2025 s.187)
                "name": "Consultation — medical services",
                "quantity": 1,
                "price_unit": 25000.00,
                "line_extension_amount": 25000.00,
                "tax_category": "ZERO_VAT",
            },
            {   # zero-rated: tuition (NTA 2025 s.187)
                "name": "Nursing tuition — tertiary",
                "quantity": 1,
                "price_unit": 150000.005,  # sub-kobo: rounds half-up
                "tax_category": "ZERO_VAT",
            },
        ],
    }
    inv.update(over)
    return inv


class TestKoboRounding(unittest.TestCase):
    def test_half_up_decimal_space(self):
        # 0.575 NGN -> 58 kobo (Go: NGNToKobo(0.575) == 58)
        self.assertEqual(ngn_to_kobo(0.575), 58)
        # float noise collapses to intended value
        self.assertEqual(ngn_to_kobo(245236.28024999998), 24523628)
        self.assertEqual(ngn_to_kobo(150000.005), 15000001)
        self.assertEqual(ngn_to_kobo(-0.575), -58)

    def test_round_bps_half_up(self):
        # mirrors model.go RoundBpsHalfUp
        self.assertEqual(round_bps_half_up(1000000, 750), 75000)   # 7.5% of N10,000
        self.assertEqual(round_bps_half_up(1, 750), 0)             # 0.075 kobo -> 0
        self.assertEqual(round_bps_half_up(1, 5000), 1)            # exactly 0.5 -> 1 (half-up)
        self.assertEqual(round_bps_half_up(0, 750), 0)
        self.assertEqual(round_bps_half_up(-1000000, 750), -75000)


class TestIRN(unittest.TestCase):
    def test_build_and_parse(self):
        irn = build_irn("INV20260001", SERVICE_ID, "2026-01-27")
        self.assertEqual(irn, "INV20260001-94ND90NR-20260127")
        num, sid, ds = parse_irn(irn)
        self.assertEqual((num, sid, ds), ("INV20260001", SERVICE_ID, "20260127"))
        self.assertTrue(valid_irn(irn))

    def test_hyphenated_invoice_number(self):
        irn = build_irn("INV-2026-0001", SERVICE_ID, "2026-01-27")
        num, sid, _ = parse_irn(irn)
        self.assertEqual(num, "INV-2026-0001")
        self.assertEqual(sid, SERVICE_ID)

    def test_rejects_bad_service_id_and_date(self):
        self.assertFalse(valid_service_id("SHORT"))
        with self.assertRaises(NRSPayloadError):
            build_irn("INV1", "SHORT", "2026-01-27")
        with self.assertRaises(NRSPayloadError):
            date_stamp("2026-13-40")
        self.assertFalse(valid_irn("nonsense"))
        self.assertFalse(valid_irn("INV-BADID-20260127"))


class TestBuildPayload(unittest.TestCase):
    def test_full_payload_shape(self):
        p = build_nrs_invoice(sample_invoice(), SERVICE_ID, BUSINESS_ID)
        self.assertEqual(p["irn"], "INV20260001-94ND90NR-20260127")
        self.assertEqual(p["business_id"], BUSINESS_ID)
        self.assertEqual(p["invoice_type_code"], "380")
        self.assertEqual(p["document_currency_code"], "NGN")
        self.assertEqual(p["payment_status"], "PENDING")
        self.assertEqual(p["buyer_reference"], "INV20260001")
        sup = p["accounting_supplier_party"]
        self.assertEqual(sup["party_name"], SUPPLIER["name"])
        self.assertEqual(sup["tin"], "12345678-0001")
        self.assertEqual(sup["postal_address"]["state"], "NG-LA")
        self.assertEqual(len(p["invoice_line"]), 3)
        self.assertEqual(p["payment_means"][0]["payment_means_code"], "30")
        self.assertEqual(p["payment_means"][0]["payment_due_date"], "2026-02-26")

    def test_totals_kobo_math(self):
        p = build_nrs_invoice(sample_invoice(), SERVICE_ID, BUSINESS_ID)
        # line 3 derived: 150000.005 -> 15000000.5 -> half-up 15000001 kobo
        excl = 1000000 + 2500000 + 15000001
        tax = round_bps_half_up(1000000, 750)  # only standard line bears VAT
        lmt = p["legal_monetary_total"]
        self.assertEqual(lmt["tax_exclusive_amount"], kobo_to_ngn(excl))
        self.assertEqual(lmt["tax_inclusive_amount"], kobo_to_ngn(excl + tax))
        self.assertEqual(lmt["payable_amount"], kobo_to_ngn(excl + tax))
        self.assertEqual(p["tax_total"][0]["tax_amount"], kobo_to_ngn(tax))
        subs = {s["tax_category"]["id"]: s for s in p["tax_total"][0]["tax_subtotal"]}
        self.assertEqual(subs["STANDARD_VAT"]["tax_category"]["percent"], 7.5)
        self.assertEqual(subs["STANDARD_VAT"]["tax_amount"], 750.0)
        self.assertEqual(subs["ZERO_VAT"]["tax_amount"], 0.0)
        self.assertNotIn("EXEMPT", subs)

    def test_credit_note_is_381(self):
        p = build_nrs_invoice(sample_invoice(move_type="out_refund",
                                             invoice_number="RINV20260001"),
                              SERVICE_ID, BUSINESS_ID)
        self.assertEqual(p["invoice_type_code"], "381")
        self.assertTrue(p["irn"].startswith("RINV20260001-"))
        # NRS expects positive amounts on credit notes
        self.assertTrue(all(l["line_extension_amount"] >= 0 for l in p["invoice_line"]))

    def test_irn_reuse_is_idempotent(self):
        irn = "INV20260001-94ND90NR-20260127"
        p = build_nrs_invoice(sample_invoice(irn=irn), SERVICE_ID, BUSINESS_ID)
        self.assertEqual(p["irn"], irn)
        with self.assertRaises(NRSPayloadError):
            build_nrs_invoice(sample_invoice(irn="bad-irn"), SERVICE_ID, BUSINESS_ID)

    def test_validation_errors_aggregated(self):
        bad = sample_invoice()
        bad["supplier"] = {"name": "", "tin": ""}
        bad["lines"] = []
        with self.assertRaises(NRSPayloadError) as ctx:
            build_nrs_invoice(bad, SERVICE_ID, BUSINESS_ID)
        msg = str(ctx.exception)
        self.assertIn("party_name", msg)
        self.assertIn("tin", msg)
        self.assertIn("at least one invoice line", msg)

    def test_bad_service_id_rejected(self):
        with self.assertRaises(NRSPayloadError):
            build_nrs_invoice(sample_invoice(), "TOO-SHORT-X", BUSINESS_ID)


if __name__ == "__main__":
    unittest.main()
