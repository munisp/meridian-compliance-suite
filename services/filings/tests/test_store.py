"""Persistence-layer tests for the filings DocStore-backed stores.

Runs against the in-memory backend (dev default); the Postgres backend is
selected by FILINGS_DATABASE_URL/DATABASE_URL and shares the same interface
(see app/store.py). Key property under test: a NEW store instance built on
the same DocStore sees previously filed records (restart durability), and ID
counters are re-seeded so durable return_ids never collide.
"""
from datetime import date, timedelta

import pytest

from app import assessment, paye, store, vat


def _vat_return(tin="12345678-0001", period="2026-01"):
    return vat.build_return(tin, period, [], {"standard_sales_kobo": 1_000_000}, [], [], 0)


def test_vat_survives_store_reinstantiation():
    docs = store.DocStore(dsn="")
    s1 = vat.VatReturnStore(docs)
    rec, created = s1.file(_vat_return(), "idem-vat-1")
    assert created
    s2 = vat.VatReturnStore(docs)  # simulate restart on same durable store
    assert s2.get("12345678-0001", "2026-01")["return_id"] == rec["return_id"]
    replay, created2 = s2.file(_vat_return(), "idem-vat-1")
    assert not created2 and replay["return_id"] == rec["return_id"]


def test_paye_survives_store_reinstantiation():
    docs = store.DocStore(dsn="")
    s1 = paye.PayeReturnStore(docs)
    sched = paye.build_monthly_schedule("87654321-0001", "2026-03",
                                        [{"tin": "11111111-0001", "name": "A",
                                          "gross_kobo": 500_000_00}])
    rec, created = s1.file(sched, "idem-paye-1")
    assert created
    s2 = paye.PayeReturnStore(docs)
    assert [r["return_id"] for r in s2.for_year("87654321-0001", 2026)] == [rec["return_id"]]
    with pytest.raises(paye.PayeError):
        s2.file(sched, "idem-paye-2")


def test_assessment_lifecycle_survives_reinstantiation():
    docs = store.DocStore(dsn="")
    s1 = assessment.AssessmentStore(docs)
    served = date(2026, 1, 10)
    a = s1.issue("12345678-0001", "vat", "2025-12", "additional",
                 5_000_000, "underdeclared output VAT", "electronic", served)
    obj = s1.object(a["assessment_id"], "wrong rate", 2_000_000, 0,
                    served + timedelta(days=5))
    s2 = assessment.AssessmentStore(docs)  # restart mid-lifecycle
    got = s2.get(a["assessment_id"])
    assert got["status"] == "objected" and got["objection_id"] == obj["objection_id"]
    dec = s2.decide(obj["objection_id"], "rejected", served + timedelta(days=20))
    assert dec["status"] == "rejected"
    s3 = assessment.AssessmentStore(docs)
    assert s3.get(a["assessment_id"])["status"] == "final_and_conclusive"


def test_tick_and_tat_referrals_durable():
    docs = store.DocStore(dsn="")
    s1 = assessment.AssessmentStore(docs)
    served = date(2026, 1, 10)
    a = s1.issue("12345678-0001", "cit", "2024", "best_of_judgment",
                 9_000_000, "failure to file", "personal", served)
    s1.object(a["assessment_id"], "grounds", 1_000_000, 0, served + timedelta(days=3))
    s2 = assessment.AssessmentStore(docs)
    events = s2.tick(served + timedelta(days=200))
    assert events and events[0]["referral_id"].startswith("TAT-")
    s3 = assessment.AssessmentStore(docs)
    assert len(s3.tat_referrals()) == 1


def test_id_counters_reseeded_from_store():
    docs = store.DocStore(dsn="")
    s1 = vat.VatReturnStore(docs)
    rec, _ = s1.file(_vat_return(period="2026-02"), "idem-vat-x")
    docs2 = store.DocStore(dsn="")
    # copy the durable record into a fresh backend, then re-seed a new store
    docs2.put("vat_returns", "12345678-0001|2026-02", rec)
    s2 = vat.VatReturnStore(docs2)
    rec2, _ = s2.file(_vat_return(period="2026-03"), "idem-vat-y")
    assert rec2["return_id"] != rec["return_id"]
