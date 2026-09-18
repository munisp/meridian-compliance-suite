"""Statutory CTR (Currency Transaction Report) pipeline (audit R4 S1b#2).

The ledger service emits `nrs.aml.ctr.v1` events for cash transactions, but
str-filing previously consumed only `nrs.aml.str.created` — statutory CTR
reporting was dark. This module consumes CTR events, aggregates them per
(tenant, counterparty, UTC day), and generates ONE statutory CTR report per
day once the cumulative cash amount reaches the threshold (NFIU/SCUML
₦10,000,000 default, env-tunable). The generated report is a normal
STRFiling with report_type="CTR", so it flows through the existing retry /
NFIU submission machinery.
"""
from __future__ import annotations

import hashlib
import json
import os
from datetime import datetime, timezone

from sqlalchemy import func
from sqlalchemy.exc import IntegrityError

from . import db

# Statutory daily cash-reporting threshold (kobo). NFIU/SCUML default:
# ₦10,000,000 = 1_000_000_000 kobo. Env override for tuning/testing; the
# value is constant-driven, not buried in the aggregation loop.
CTR_THRESHOLD_KOBO = int(os.environ.get("CTR_THRESHOLD_KOBO", "1000000000"))

TOPIC_CTR = "nrs.aml.ctr.v1"


def _day_of(occurred_at: str | None) -> str:
    if occurred_at:
        # accept ISO-8601 (date or datetime); the statutory aggregation
        # window is the UTC calendar day.
        try:
            return datetime.fromisoformat(
                occurred_at.replace("Z", "+00:00")).astimezone(
                    timezone.utc).date().isoformat()
        except ValueError:
            pass
        if len(occurred_at) >= 10 and occurred_at[4] == "-" and occurred_at[7] == "-":
            return occurred_at[:10]
        raise ValueError(f"occurred_at {occurred_at!r} is not ISO-8601")
    return datetime.now(timezone.utc).date().isoformat()


def intake_ctr(event: dict, *, actor: str) -> tuple[dict, bool]:
    """Consume one nrs.aml.ctr.v1 event. Idempotent per (tenant,
    idempotency_key). Returns (report, created): when the counterparty's
    cumulative cash for the day reaches CTR_THRESHOLD_KOBO, exactly one
    statutory CTR filing is generated for (tenant, counterparty, day);
    `created` is True only for that first generation."""
    tenant_id = str(event.get("tenant_id") or "")
    idem = str(event.get("idempotency_key") or "")
    counterparty = str(event.get("counterparty_ref") or event.get("subject_ref") or "")
    if not idem:
        raise ValueError("idempotency_key is required")
    if not counterparty:
        raise ValueError("counterparty_ref is required")
    amount = int(event.get("amount_kobo") or 0)
    if amount <= 0:
        raise ValueError("amount_kobo must be positive")
    day = _day_of(event.get("occurred_at"))
    actor = str(event.get("actor") or actor or "aml-ctr-bus")

    # Lazy: main imports this module for bus dispatch, so main's helpers are
    # imported at call time (no circular import).
    from .main import _canonical_payload, audit, sessions
    with sessions() as s:
        # Idempotent event intake.
        dup = (s.query(db.CTREvent)
               .filter_by(tenant_id=tenant_id, idempotency_key=idem)
               .one_or_none())
        if dup is not None:
            filing = (s.query(db.STRFiling)
                      .filter_by(tenant_id=tenant_id,
                                 idempotency_key=_ctr_idem(tenant_id, counterparty, day))
                      .one_or_none())
            s.commit()
            return (_with_payload(filing) if filing else {}), False
        s.add(db.CTREvent(
            tenant_id=tenant_id, idempotency_key=idem,
            counterparty_ref=counterparty, amount_kobo=amount,
            currency=str(event.get("currency") or "NGN"),
            occurred_day=day,
            occurred_at=str(event.get("occurred_at") or ""),
            account_ref=str(event.get("account_ref") or ""),
            channel=str(event.get("channel") or ""),
            raw=json.dumps(event, sort_keys=True)))
        try:
            s.flush()
        except IntegrityError:
            # Lost the same-key race: replay the winner (never a 500).
            s.rollback()
            filing = (s.query(db.STRFiling)
                      .filter_by(tenant_id=tenant_id,
                                 idempotency_key=_ctr_idem(tenant_id, counterparty, day))
                      .one_or_none())
            return (_with_payload(filing) if filing else {}), False

        total = (s.query(func.coalesce(func.sum(db.CTREvent.amount_kobo), 0))
                 .filter_by(tenant_id=tenant_id, counterparty_ref=counterparty,
                            occurred_day=day)
                 .scalar())
        report_idem = _ctr_idem(tenant_id, counterparty, day)
        existing = (s.query(db.STRFiling)
                    .filter_by(tenant_id=tenant_id, idempotency_key=report_idem)
                    .one_or_none())
        if total < CTR_THRESHOLD_KOBO or existing is not None:
            s.commit()
            return (_with_payload(existing) if existing else {}), False

        count = (s.query(func.count(db.CTREvent.id))
                 .filter_by(tenant_id=tenant_id, counterparty_ref=counterparty,
                            occurred_day=day)
                 .scalar())
        payload = {
            "report_kind": "CTR",
            "counterparty_ref": counterparty,
            "date": day,
            "total_cash_kobo": int(total),
            "transaction_count": int(count),
            "currency": "NGN",
            "threshold_kobo": CTR_THRESHOLD_KOBO,
        }
        payload_raw = _canonical_payload(payload)
        f = db.STRFiling(
            tenant_id=tenant_id, idempotency_key=report_idem,
            subject_ref=counterparty, report_type="CTR",
            payload=payload_raw,
            payload_hash=hashlib.sha256(payload_raw.encode()).hexdigest(),
            created_by=actor)
        s.add(f)
        try:
            s.flush()
        except IntegrityError:
            # Lost the report race: the other intake generated it first.
            s.rollback()
            winner = (s.query(db.STRFiling)
                      .filter_by(tenant_id=tenant_id, idempotency_key=report_idem)
                      .one_or_none())
            return (_with_payload(winner) if winner else {}), False
        audit.record(
            actor=actor, str_id=f.id, tenant_id=tenant_id, old_status="",
            new_status=db.STATUS_PENDING, str_hash=f.payload_hash,
            detail=(f"ctr.generate counterparty={counterparty} day={day} "
                    f"total_kobo={int(total)} threshold_kobo={CTR_THRESHOLD_KOBO}"))
        s.commit()
        return {**f.to_dict(), "payload": payload}, True


def _ctr_idem(tenant_id: str, counterparty: str, day: str) -> str:
    return f"ctr:{tenant_id}:{counterparty}:{day}"


def _with_payload(filing: db.STRFiling) -> dict:
    """to_dict plus the parsed aggregate payload (report consumers need the
    totals, not just the hash)."""
    d = filing.to_dict()
    try:
        d["payload"] = json.loads(filing.payload)
    except Exception:
        d["payload"] = {}
    return d
