"""Regression (R4 S1b + #11):
(1) A REJECTED objection previously made the assessment final-and-conclusive
    instantly — no TAT appeal window. Now: appealable with a 30-day window,
    final only after lapse or a TAT decision.
(2) Statute-barred assessments (older than the limitation period per tax
    type) remained enforceable. Now: time-barred assessments are marked
    non-enforceable (payment demand refused, sweeper marks them) while the
    record stays readable."""
from __future__ import annotations

from datetime import date, timedelta

import pytest

from app.assessment import (APPEAL_WINDOW_DAYS, AssessmentError,
                            AssessmentStore, limitation_expiry)

T0 = date(2026, 1, 1)


def _rejected(store: AssessmentStore):
    a = store.issue("TIN-1", "CIT", "2025", "additional", 5_000_000_00,
                    "under-declared income", "electronic", T0)
    obj = store.object(a["assessment_id"], "wrong figures", 1_000_000_00,
                       1_000_000_00, T0 + timedelta(days=5))
    store.decide(obj["objection_id"], "rejected", T0 + timedelta(days=20))
    return a["assessment_id"]


def test_rejected_objection_is_appealable_not_final():
    s = AssessmentStore()
    aid = _rejected(s)
    a = s.get(aid)
    assert a["status"] == "appealable"  # NOT final_and_conclusive
    assert a["appeal_deadline"] == (T0 + timedelta(days=20 + APPEAL_WINDOW_DAYS)).isoformat()
    # Enforcement is blocked while appealable.
    with pytest.raises(AssessmentError, match="not yet enforceable"):
        s.demand_payment(aid, T0 + timedelta(days=25))


def test_appeal_then_tat_decision_makes_final():
    s = AssessmentStore()
    aid = _rejected(s)
    appeal = s.appeal(aid, "TAT grounds", T0 + timedelta(days=25))
    assert appeal["status"] == "pending"
    assert s.get(aid)["status"] == "under_appeal"
    s.tat_decision(appeal["appeal_id"], "varied", T0 + timedelta(days=60),
                   varied_amount_kobo=3_000_000_00)
    a = s.get(aid)
    assert a["status"] == "final_and_conclusive"
    assert a["amount_kobo"] == 3_000_000_00


def test_appeal_window_lapse_finalises():
    s = AssessmentStore()
    aid = _rejected(s)
    # Late appeal refused.
    with pytest.raises(AssessmentError, match="out of time"):
        s.appeal(aid, "late", T0 + timedelta(days=20 + APPEAL_WINDOW_DAYS + 1))
    events = s.tick(T0 + timedelta(days=20 + APPEAL_WINDOW_DAYS + 1))
    assert s.get(aid)["status"] == "final_and_conclusive"
    assert any(e["event"] == "final_and_conclusive" for e in events)


def test_statute_barred_assessment_enforcement_refused():
    s = AssessmentStore()
    # Period 2019 with a 6-year CIT limitation: expiry 2025-12-31.
    assert limitation_expiry("2019", "CIT") == date(2025, 12, 31)
    a = s.issue("TIN-2", "CIT", "2019", "additional", 2_000_000_00,
                "old assessment", "registered_post", T0)
    # Lazy check at the enforcement point: demand refused, marked barred,
    # record stays readable.
    with pytest.raises(AssessmentError, match="statute-barred"):
        s.demand_payment(a["assessment_id"], T0)
    rec = s.get(a["assessment_id"])
    assert rec is not None and rec["status"] == "statute_barred"
    assert rec["history"][-1]["event"] == "statute_barred"
    with pytest.raises(AssessmentError, match="statute-barred"):
        s.demand_payment(a["assessment_id"], T0)


def test_sweeper_marks_time_barred_and_skips_fresh():
    s = AssessmentStore()
    old = s.issue("TIN-3", "VAT", "2018-06", "additional", 100_000_00,
                  "old VAT", "personal", T0)
    fresh = s.issue("TIN-3", "VAT", "2025-12", "additional", 100_000_00,
                    "fresh VAT", "personal", T0)
    events = s.tick(T0)
    assert s.get(old["assessment_id"])["status"] == "statute_barred"
    assert s.get(fresh["assessment_id"])["status"] == "open"
    assert any(e.get("event") == "statute_barred" for e in events)


def test_enforceable_final_assessment_demands_payment():
    s = AssessmentStore()
    a = s.issue("TIN-4", "CIT", "2025", "additional", 4_000_000_00,
                "recent", "electronic", T0)
    s.tick(T0 + timedelta(days=40))  # objection window lapses -> final
    out = s.demand_payment(a["assessment_id"], T0 + timedelta(days=45))
    assert out["amount_due_kobo"] == 4_000_000_00
