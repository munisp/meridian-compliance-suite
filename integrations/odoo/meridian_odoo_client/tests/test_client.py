"""Client tests with a mocked transport — no HTTP, no Odoo, no Go service."""

import json
import unittest

from meridian_odoo_client import MeridianAPIError, MeridianClient

BASE = "https://meridian.example.test"
IRN = "INV20260001-94ND90NR-20260127"

LIFECYCLE_RESPONSE = {
    "irn": IRN,
    "status": "confirmed",
    "invoice_id": "inv_abc123",
    "payment_status": "PENDING",
    "crypto_stamp": {"algorithm": "ed25519", "key_id": "keyx-dev"},
    "qr": {"payload": "NRS-QR|...", "signature": "ab12", "qr_svg": "<svg/>"},
    "steps": [
        {"name": n, "status": "ok"} for n in (
            "1-create-store", "2-irn-generate", "3-irn-validate", "4-irn-sign",
            "5-schema-validate", "6-invoice-sign", "7-transmit", "8-confirm",
        )
    ],
}


class FakeTransport:
    def __init__(self, status=201, response=None):
        self.status = status
        self.response = response if response is not None else LIFECYCLE_RESPONSE
        self.calls = []

    def __call__(self, method, url, body, headers):
        self.calls.append({
            "method": method, "url": url,
            "body": json.loads(body) if body else None,
            "headers": headers,
        })
        return self.status, self.response


def make_client(transport):
    return MeridianClient(BASE, api_key="sekret", service_id="94ND90NR",
                          business_id="biz-odoo-001", transport=transport)


class TestSubmit(unittest.TestCase):
    def test_submit_posts_payload_with_auth(self):
        t = FakeTransport()
        c = make_client(t)
        resp = c.submit_invoice({"business_id": "biz-odoo-001", "irn": IRN},
                                idempotency_key="odoo-move-42")
        self.assertEqual(resp["irn"], IRN)
        self.assertEqual(resp["status"], "confirmed")
        self.assertEqual(len(resp["steps"]), 8)
        call = t.calls[0]
        self.assertEqual(call["method"], "POST")
        self.assertEqual(call["url"], BASE + "/v1/invoices/nrs")
        self.assertEqual(call["headers"]["Authorization"], "Bearer sekret")
        self.assertEqual(call["headers"]["Idempotency-Key"], "odoo-move-42")
        self.assertEqual(call["body"]["irn"], IRN)

    def test_submit_422_raises_with_error_list(self):
        t = FakeTransport(status=422, response={
            "type": "about:blank", "title": "NRS schema validation failed",
            "status": 422,
            "errors": [{"field": "accounting_supplier_party.tin",
                        "code": "REQUIRED",
                        "message": "accounting_supplier_party tin is required"}],
        })
        c = make_client(t)
        with self.assertRaises(MeridianAPIError) as ctx:
            c.submit_invoice({"business_id": "biz-odoo-001"})
        self.assertEqual(ctx.exception.status, 422)
        self.assertIn("accounting_supplier_party.tin", str(ctx.exception))

    def test_connection_error(self):
        def boom(method, url, body, headers):
            raise MeridianAPIError("connection to Meridian service failed: refused")
        c = make_client(boom)
        with self.assertRaises(MeridianAPIError):
            c.submit_invoice({})


class TestPaymentStatusPatch(unittest.TestCase):
    def test_patch_by_irn(self):
        t = FakeTransport(status=200, response={"irn": IRN, "payment_status": "PAID"})
        c = make_client(t)
        resp = c.update_payment_status(IRN, "PAID", payment_reference="PAY-99")
        self.assertEqual(resp["payment_status"], "PAID")
        call = t.calls[0]
        self.assertEqual(call["method"], "PATCH")
        self.assertEqual(call["url"], BASE + "/v1/invoices/" + IRN)
        self.assertEqual(call["body"], {"payment_status": "PAID",
                                        "payment_reference": "PAY-99"})


class TestWebhookRegistration(unittest.TestCase):
    def test_register(self):
        t = FakeTransport(status=201, response={"url": "https://odoo.example/nrs/webhook"})
        c = make_client(t)
        c.register_webhook("https://odoo.example/nrs/webhook", "whsec-0123456789abcdef")
        call = t.calls[0]
        self.assertEqual(call["body"]["business_id"], "biz-odoo-001")
        self.assertEqual(call["body"]["secret"], "whsec-0123456789abcdef")


if __name__ == "__main__":
    unittest.main()
