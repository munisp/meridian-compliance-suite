"""Regression (R4 S1a#5): WHT was deducted on the VAT-INCLUSIVE gross.
VAT is a pass-through tax, not income — per FIRS practice the WHT base is
the net-of-VAT consideration. The pack rule wht.base.vat-exclusive now
governs: when includes_vat is set, the engine deducts on
amount_kobo - vat_kobo; without the pack rule the legacy gross base would
apply (pack values govern)."""
from __future__ import annotations

from app import engine as wht_engine


def _req(**kw):
    base = {
        "payment_type": "services",
        "beneficiary": "company",
        "amount_kobo": 10_750_000,  # N107,500.00 = N100,000 net + 7.5% VAT
        "supplier_tin": "1234567890123",
    }
    base.update(kw)
    return base


def test_vat_inclusive_deducts_on_net_of_vat():
    res = wht_engine.evaluate_wht(_req(includes_vat=True, vat_kobo=750_000))
    # services company rate 5% -> 5% of N100,000.00 net, NOT of N107,500 gross
    assert res["wht_base_kobo"] == 10_000_000
    assert res["vat_kobo"] == 750_000
    assert res["wht_kobo"] == wht_engine.round_half_up_kobo(10_000_000, res["rate_bps"])
    # Vendor receives the gross less WHT (VAT passes through to FIRS).
    assert res["net_payable_kobo"] == 10_750_000 - res["wht_kobo"]
    assert "wht.base.vat-exclusive" in res["matched_rules"]


def test_no_vat_component_keeps_full_base():
    res = wht_engine.evaluate_wht(_req())
    assert res["wht_base_kobo"] == 10_750_000
    assert res["wht_kobo"] == wht_engine.round_half_up_kobo(10_750_000, res["rate_bps"])
    assert "wht.base.vat-exclusive" not in res["matched_rules"]


def test_includes_vat_requires_positive_vat_kobo():
    import pytest
    with pytest.raises(ValueError, match="vat_kobo"):
        wht_engine.evaluate_wht(_req(includes_vat=True))
    with pytest.raises(ValueError, match="vat_kobo"):
        wht_engine.evaluate_wht(_req(includes_vat=True, vat_kobo=10_750_000))


def test_pack_rule_absent_falls_back_to_gross():
    """Pack values govern: with the rule dropped from the pack, the legacy
    gross base applies (a pack release controls the behaviour, not code)."""
    pack = wht_engine._registry.load("rp-wht-2024")
    rules = [r for r in pack.rules if r.get("id") != "wht.base.vat-exclusive"]
    p2 = wht_engine.Pack(id=pack.id, version=pack.version,
                         effective_from=pack.effective_from,
                         effective_to=pack.effective_to, status=pack.status,
                         subject_to_regazette=pack.subject_to_regazette,
                         provenance=pack.provenance, signed=pack.signed,
                         rules=rules, raw=pack.raw)
    res = wht_engine.evaluate_wht(_req(includes_vat=True, vat_kobo=750_000), pack=p2)
    assert res["wht_base_kobo"] == 10_750_000  # gross (rule absent)
    assert "wht.base.vat-exclusive" not in res["matched_rules"]
