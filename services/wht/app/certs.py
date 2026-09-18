"""WHT vendor credit certificates + over-deduction refunds (audit R4 S1a#10).

WHT Regs 2024 reg. 7: the deducting agent MUST issue a WHT credit note to
the vendor for every deduction. This module issues a signed certificate per
deduction (HMAC-SHA256 over the canonical payload; vendor-retrievable) and
routes over-deductions into the existing credit/refund machinery: when a
re-evaluation shows less WHT was due than was deducted, the difference is
credited to the vendor's credit ledger (applyable via the existing
/v1/wht/credits/apply path).
"""
from __future__ import annotations

import hashlib
import hmac
import json
import os
import uuid

from . import db, engine as wht_engine

_CERT_KEY = os.environ.get("WHT_CERT_KEY", "meridian-dev-wht-cert")


def _canonical(payload: dict) -> str:
    return json.dumps(payload, sort_keys=True, separators=(",", ":"))


def sign(payload: dict) -> str:
    return hmac.new(_CERT_KEY.encode(), _canonical(payload).encode(),
                    hashlib.sha256).hexdigest()


def issue_certificate(sess, deduction: db.Deduction) -> db.Certificate:
    """Issue (idempotently) the reg. 7 credit certificate for a deduction."""
    existing = (sess.query(db.Certificate)
                .filter_by(deduction_id=deduction.id).one_or_none())
    if existing is not None:
        return existing
    payload = {
        "deduction_id": deduction.id,
        "tenant_id": deduction.tenant_id or "",
        "vendor_tin": deduction.vendor_tin,
        "vendor_name": deduction.vendor_name or "",
        "payment_type": deduction.payment_type,
        "beneficiary": deduction.beneficiary,
        "amount_kobo": deduction.amount_kobo,
        "rate_bps": deduction.rate_bps,
        "wht_kobo": deduction.wht_kobo,
        "deduction_date": deduction.deduction_date,
        "period": deduction.period,
    }
    cert = db.Certificate(
        id=f"cert-{uuid.uuid4().hex[:12]}",
        deduction_id=deduction.id,
        tenant_id=deduction.tenant_id or "",
        vendor_tin=deduction.vendor_tin,
        payload=_canonical(payload),
        signature=sign(payload),
        issued_at=db.now(),
    )
    sess.add(cert)
    sess.flush()
    return cert


def certificate_view(cert: db.Certificate) -> dict:
    payload = json.loads(cert.payload)
    return {
        "certificate_id": cert.id,
        "issued_at": cert.issued_at,
        "signature": cert.signature,
        "signature_algorithm": "HMAC-SHA256",
        "regulation": "WHT Regs 2024, reg. 7 (credit note)",
        **payload,
    }


def verify_certificate(cert: db.Certificate) -> bool:
    return hmac.compare_digest(cert.signature, sign(json.loads(cert.payload)))


def process_refund(sess, deduction: db.Deduction, corrected_req: dict,
                   *, actor: str, idempotency_key: str = "") -> db.Refund:
    """Over-deduction refund: re-evaluate the deduction with corrected facts;
    the excess WHT is credited to the vendor's credit ledger (existing
    refund machinery). Idempotent per (deduction, idempotency_key)."""
    rid = ("ref-" + hashlib.sha256(
        f"idem:{idempotency_key}".encode()).hexdigest()[:12]
        if idempotency_key else f"ref-{uuid.uuid4().hex[:12]}")
    existing = sess.get(db.Refund, rid)
    if existing is not None:
        if existing.deduction_id != deduction.id:
            raise ValueError("idempotency_key reused for a different deduction")
        return existing
    corrected = wht_engine.evaluate_wht(corrected_req)
    over = int(deduction.wht_kobo) - int(corrected["wht_kobo"])
    if over <= 0:
        raise ValueError(
            f"no over-deduction: corrected WHT {corrected['wht_kobo']} kobo "
            f">= deducted {deduction.wht_kobo} kobo")
    credit = db.Credit(
        id=f"cr-{uuid.uuid4().hex[:12]}",
        tenant_id=deduction.tenant_id or "",
        vendor_tin=deduction.vendor_tin,
        credit_kobo=over,
        source=f"refund:{deduction.id}",
        period=deduction.period or "",
        note="WHT over-deduction refund (audit R4 S1a#10)",
        created_at=db.now(),
    )
    sess.add(credit)
    sess.flush()
    refund = db.Refund(
        id=rid,
        tenant_id=deduction.tenant_id or "",
        deduction_id=deduction.id,
        vendor_tin=deduction.vendor_tin,
        deducted_wht_kobo=int(deduction.wht_kobo),
        corrected_wht_kobo=int(corrected["wht_kobo"]),
        over_deducted_kobo=over,
        credit_id=credit.id,
        status="credited",
        created_by=actor,
        created_at=db.now(),
    )
    sess.add(refund)
    sess.flush()
    return refund
