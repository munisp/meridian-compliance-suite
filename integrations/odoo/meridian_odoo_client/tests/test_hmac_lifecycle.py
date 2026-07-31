"""Webhook HMAC accept/reject + lifecycle state transition tests."""

import json
import unittest

from meridian_odoo_client import (
    LIFECYCLE_STEPS,
    map_service_status,
    next_status,
    sign_webhook,
    valid_transition,
    verify_webhook_signature,
)

SECRET = "whsec-0123456789abcdef"


def webhook_body():
    return json.dumps({
        "event": "nrs.einvoice.transmitted.v1",
        "business_id": "biz-odoo-001",
        "sent_at": "2026-01-27T10:00:00Z",
        "data": {"irn": "INV20260001-94ND90NR-20260127",
                 "invoice_id": "inv_abc123", "status": "transmitted"},
    }).encode("utf-8")


class TestHMAC(unittest.TestCase):
    def test_sign_matches_stdlib_hmac(self):
        # Same primitive as Go's SignWebhook (HMAC-SHA256 hex over raw body);
        # stdlib hmac/hashlib is the reference implementation here.
        import hashlib
        import hmac as std_hmac
        want = std_hmac.new(b"secret", b"hello", hashlib.sha256).hexdigest()
        self.assertEqual(sign_webhook("secret", b"hello"), want)
        self.assertEqual(sign_webhook("secret", "hello"), want)  # str accepted

    def test_accept_valid_signature(self):
        body = webhook_body()
        sig = sign_webhook(SECRET, body)
        self.assertTrue(verify_webhook_signature(SECRET, body, sig))
        self.assertTrue(verify_webhook_signature(SECRET, body, sig.upper()))

    def test_reject_wrong_secret_body_signature(self):
        body = webhook_body()
        sig = sign_webhook(SECRET, body)
        self.assertFalse(verify_webhook_signature("other-secret", body, sig))
        self.assertFalse(verify_webhook_signature(SECRET, body + b" ", sig))
        self.assertFalse(verify_webhook_signature(SECRET, body, "0" * 64))
        self.assertFalse(verify_webhook_signature(SECRET, body, ""))
        self.assertFalse(verify_webhook_signature("", body, sig))
        self.assertFalse(verify_webhook_signature(SECRET, b"", sig))

    def test_reject_tampered_event(self):
        body = webhook_body()
        sig = sign_webhook(SECRET, body)
        tampered = body.replace(b"transmitted", b"confirmed")
        self.assertFalse(verify_webhook_signature(SECRET, tampered, sig))


class TestLifecycle(unittest.TestCase):
    def test_eight_steps_defined(self):
        self.assertEqual(len(LIFECYCLE_STEPS), 8)
        self.assertEqual(LIFECYCLE_STEPS[0], "1-create-store")
        self.assertEqual(LIFECYCLE_STEPS[-1], "8-confirm")

    def test_map_service_status(self):
        self.assertEqual(map_service_status("received"), "submitted")
        self.assertEqual(map_service_status("signed"), "signed")
        self.assertEqual(map_service_status("transmitted"), "transmitted")
        self.assertEqual(map_service_status("confirmed"), "confirmed")
        self.assertEqual(map_service_status("failed"), "failed")
        self.assertEqual(map_service_status(""), "submitted")

    def test_happy_path(self):
        s = "draft"
        for ev in ("submitted", "signed", "transmitted", "confirmed"):
            s = next_status(s, ev)
        self.assertEqual(s, "confirmed")

    def test_failure_and_retry(self):
        s = next_status("submitted", "failed")
        self.assertEqual(s, "failed")
        s = next_status(s, "retry")
        self.assertEqual(s, "submitted")

    def test_illegal_transitions_rejected(self):
        self.assertFalse(valid_transition("draft", "confirmed"))
        self.assertFalse(valid_transition("confirmed", "failed"))
        self.assertFalse(valid_transition("confirmed", "transmitted"))
        with self.assertRaises(ValueError):
            next_status("draft", "confirmed")
        with self.assertRaises(ValueError):
            next_status("confirmed", "transmitted")


if __name__ == "__main__":
    unittest.main()
