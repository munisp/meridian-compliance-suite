"""Regression (R4 S1b#2): nrs.aml.ctr.v1 events were emitted by the ledger
but str-filing consumed only nrs.aml.str.created — statutory CTR reporting
was dark. CTR events are now consumed, aggregated per counterparty/day, and
exactly one statutory CTR report is generated when the daily cumulative
cash amount reaches the threshold."""
from __future__ import annotations

from app import ctr, db, main


def _ev(idem, cp="CP-1", amount=400_000_000, day="2026-01-15"):
    return {"tenant_id": "tenant-aml", "idempotency_key": idem,
            "counterparty_ref": cp, "amount_kobo": amount,
            "occurred_at": day + "T10:00:00Z"}


def test_ctr_below_threshold_no_report():
    report, created = ctr.intake_ctr(_ev("k1", cp="CP-BELOW"), actor="t")
    assert created is False and report == {}


def test_ctr_crossing_generates_one_statutory_report():
    # 3 x N4,000,000 cash = N12,000,000 > N10,000,000 threshold
    r1, c1 = ctr.intake_ctr(_ev("a1"), actor="t")
    assert not c1 and r1 == {}
    r2, c2 = ctr.intake_ctr(_ev("a2"), actor="t")
    assert not c2 and r2 == {}
    r3, c3 = ctr.intake_ctr(_ev("a3"), actor="t")
    assert c3 is True
    assert r3["report_type"] == "CTR"
    assert r3["subject_ref"] == "CP-1"
    assert r3["payload"]["total_cash_kobo"] == 1_200_000_000
    assert r3["payload"]["transaction_count"] == 3
    assert r3["payload"]["threshold_kobo"] == ctr.CTR_THRESHOLD_KOBO
    # further same-day events: aggregated but NO second report
    r4, c4 = ctr.intake_ctr(_ev("a4"), actor="t")
    assert c4 is False
    assert r4["idempotency_key"] == r3["idempotency_key"]
    # The generated CTR report flows through the existing worker -> NFIU
    # submission machinery (filed, not left pending).
    from app import main as _m
    _m.worker.process_due()
    with main.sessions() as s:
        f = (s.query(db.STRFiling)
             .filter_by(tenant_id="tenant-aml", report_type="CTR").one())
        assert f.status == db.STATUS_FILED
    with main.sessions() as s:
        n = (s.query(db.STRFiling)
             .filter_by(tenant_id="tenant-aml", report_type="CTR").count())
        assert n == 1


def test_ctr_idempotent_event_replay():
    ctr.intake_ctr(_ev("b1", cp="CP-REPLAY"), actor="t")
    report, created = ctr.intake_ctr(_ev("b1", cp="CP-REPLAY"), actor="t")  # replay
    assert created is False
    with main.sessions() as s:
        n = (s.query(db.CTREvent)
             .filter_by(tenant_id="tenant-aml", idempotency_key="b1").count())
        assert n == 1  # event stored once


def test_ctr_per_counterparty_day_isolation():
    # Different counterparty and different day aggregate independently.
    ctr.intake_ctr(_ev("d1", cp="CP-2"), actor="t")
    report, created = ctr.intake_ctr(_ev("d2", cp="CP-2", day="2026-01-16"),
                                     actor="t")
    assert created is False  # CP-2 has only N4m each day
