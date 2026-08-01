"""HMAC-SHA256 webhook signature helpers — exact mirror of
services/einvoicing/webhooks.go SignWebhook / VerifyWebhookSignature.

The Meridian service sends header ``X-Meridian-Signature: <hex hmac>``
where the MAC key is the endpoint secret and the message is the raw
request body. Comparison is constant-time.
"""

from __future__ import annotations

import hashlib
import hmac


def sign_webhook(secret: str, body: bytes) -> str:
    """Compute the X-Meridian-Signature value (HMAC-SHA256 hex)."""
    if isinstance(body, str):
        body = body.encode("utf-8")
    return hmac.new(secret.encode("utf-8"), body, hashlib.sha256).hexdigest()


def verify_webhook_signature(secret: str, body: bytes, signature: str) -> bool:
    """Constant-time check of a delivered signature. Rejects empty inputs."""
    if not secret or not body or not signature:
        return False
    want = sign_webhook(secret, body)
    return hmac.compare_digest(want, signature.strip().lower())
