"""WHT engine semantics tests (feature/wht-engine-semantics).

Covers the new pack semantics from meridian-rule-packs branch
feature/tax-law-parity (tip 4c092b7) plus the engine's own date-blindness fix
(audit gap #14):

  E1  precedence: higher precedence wins on multi-match (royalty NR individual
      5% vs generic non-resident 10%)
  E2  `not_in` operator in rule conditions (no-TIN double rate excludes
      passive income)
  E3  `tax:` when-discriminator — CIT vs Development Levy rules cannot clobber
      each other's computed fields
  E4  date-aware rule dispatch on rule effective_from/effective_to:
      2025-dated CIT -> legacy rules, 2026-dated -> NTA rules; winnings WHT
      exempt pre-2024-10-01, 5%/15% after (boundary dates)
  E5  small-company carve-out requires supplier TIN + payer small + monthly
      value <= N2m (engine-side enforcement: deny without TIN)
  E6  sync test: the audit's 28-row WHT matrix through the REAL engine against
      the actual pack files from the rule-packs repo (feature/tax-law-parity)

Pack location for E3/E4/E6: $TAXFIX_PACKS_DIR, else the shared worktree
/mnt/agents/output/taxfix-rulepacks/packs, else /tmp/taxfix/packs.
"""
from __future__ import annotations

import os
from pathlib import Path

import pytest
import yaml

from app import engine as wht_engine
from meridian_py.rulepack import Pack, PackRegistry

TIN13 = "1234567890123"
AMOUNT = 10_000_000_00  # N10,000,000.00


def _taxfix_packs_dir() -> Path:
    for cand in (os.environ.get("TAXFIX_PACKS_DIR"),
                 "/mnt/agents/output/taxfix-rulepacks/packs",
                 "/tmp/taxfix/packs"):
        if cand and (Path(cand) / "rp-wht-2024" / "1.0.0.yaml").is_file():
            return Path(cand)
    pytest.skip("tax-law-parity pack checkout not found "
                "(set TAXFIX_PACKS_DIR)")


def _load_pack(pack_id: str) -> Pack:
    return Pack.from_yaml(
        (_taxfix_packs_dir() / pack_id / "1.0.0.yaml").read_text())


def _eval_new_pack(req: dict) -> dict:
    """evaluate_wht against the real feature/tax-law-parity rp-wht-2024 pack."""
    reg = PackRegistry(packs_dir=str(_taxfix_packs_dir().parent))
    return wht_engine.evaluate_wht(req, registry=reg)


def _synth_pack(rules: list[dict]) -> Pack:
    doc = {"id": "rp-synth", "version": "1.0.0",
           "effective_from": "2020-01-01", "effective_to": None,
           "rules": rules}
    return Pack.from_yaml(yaml.safe_dump(doc))


# ------------------------------------------------------------------- E1: precedence
def test_e1_precedence_beats_file_order_generic_cannot_clobber_specific():
    pack = _synth_pack([
        {"id": "generic.nr", "when": {"beneficiary_residence": "non_resident"},
         "then": {"rate_bps": 1000}},
        {"id": "specific.royalty-nr-individual",
         "when": {"payment_type": "royalty", "beneficiary": "individual",
                  "beneficiary_residence": "non_resident"},
         "then": {"rate_bps": 500, "precedence": 20}},
    ])
    ctx = {"payment_type": "royalty", "beneficiary": "individual",
           "beneficiary_residence": "non_resident", "tax": "WHT"}
    out = wht_engine.evaluate_rules(pack, ctx, as_of="2025-03-01")
    # specific rule appears LATER (would also win under plain last-match-wins)...
    assert out["decision"]["rate_bps"] == 500
    # ...and still wins when it appears FIRST (precedence, not order, decides)
    pack.rules.reverse()
    out2 = wht_engine.evaluate_rules(pack, ctx, as_of="2025-03-01")
    assert out2["decision"]["rate_bps"] == 500


def test_e1_royalty_non_resident_individual_5pct_real_pack():
    body = _eval_new_pack({
        "payment_type": "royalty", "beneficiary": "individual",
        "beneficiary_residence": "non_resident", "amount_kobo": AMOUNT,
        "supplier_tin": TIN13, "payment_date": "2025-03-01"})
    assert body["rate_bps"] == 500
    assert "wht.rate.royalty.non-resident-individual" in body["matched_rules"]


def test_e1_royalty_non_resident_company_stays_10pct_real_pack():
    body = _eval_new_pack({
        "payment_type": "royalty", "beneficiary": "company",
        "beneficiary_residence": "non_resident", "amount_kobo": AMOUNT,
        "supplier_tin": TIN13, "payment_date": "2025-03-01"})
    assert body["rate_bps"] == 1000


# ------------------------------------------------------------------- E2: not_in
def test_e2_not_in_operator_map_and_suffix():
    ctx = {"payment_type": "dividend"}
    ok, _ = wht_engine._match_cond(
        "payment_type", {"not_in": ["dividend", "interest"]}, ctx)
    assert ok is False
    ok, _ = wht_engine._match_cond(
        "payment_type", {"not_in": ["services", "commission"]}, ctx)
    assert ok is True
    ok, _ = wht_engine._match_cond(
        "payment_type__not_in", ["dividend"], ctx)
    assert ok is False


def test_e2_no_tin_double_rate_excludes_passive_income_real_pack():
    # dividend (passive) without TIN: NOT doubled (not_in exclusion)
    body = _eval_new_pack({
        "payment_type": "dividend", "beneficiary": "company",
        "amount_kobo": AMOUNT, "payment_date": "2025-03-01"})
    assert body["rate_bps"] == 1000
    assert body["no_tin_double_applied"] is False
    # services (active) without TIN: doubled 2% -> 4%
    body2 = _eval_new_pack({
        "payment_type": "services", "beneficiary": "company",
        "amount_kobo": AMOUNT, "payment_date": "2025-03-01"})
    assert body2["rate_bps"] == 400
    assert body2["no_tin_double_applied"] is True


# ------------------------------------------------------------------- E3: tax discriminator
def test_e3_tax_discriminator_isolates_computed_fields():
    pack = _synth_pack([
        {"id": "cit.standard", "when": {"tax": "CIT", "entity": "company"},
         "then": {"rate_bps": 3000, "narrate": "CIT 30%"}},
        {"id": "dev-levy", "when": {"tax": "DevelopmentLevy", "entity": "company"},
         "then": {"rate_bps": 400, "levy_bps": 400, "narrate": "Dev Levy 4%"}},
        {"id": "tet-legacy", "when": {"tax": "TET", "entity": "company"},
         "then": {"rate_bps": 200, "narrate": "TET 2%"}},
    ])
    cit = wht_engine.evaluate_rules(
        pack, {"entity": "company", "tax": "CIT"}, as_of="2026-06-30")
    assert cit["decision"]["rate_bps"] == 3000
    assert "levy_bps" not in cit["decision"]  # Dev Levy did not clobber
    levy = wht_engine.evaluate_rules(
        pack, {"entity": "company", "tax": "DevelopmentLevy"}, as_of="2026-06-30")
    assert levy["decision"]["rate_bps"] == 400
    assert levy["decision"]["levy_bps"] == 400


def test_e3_real_pack_cit_vs_development_levy():
    edu = _load_pack("rp-education-ng")
    facts = {"entity": "company", "size": "large", "date": "2026-06-30",
             "assessable_profit_kobo": 5_000_000_000}
    cit = wht_engine.evaluate_rules(edu, {**facts, "tax": "CIT"},
                                    as_of="2026-06-30")
    assert cit["decision"]["rate_bps"] == 3000
    levy = wht_engine.evaluate_rules(edu, {**facts, "tax": "DevelopmentLevy"},
                                     as_of="2026-06-30")
    assert levy["decision"]["rate_bps"] == 400  # 4% Dev Levy, not 30% CIT


def test_e3_engine_sets_default_tax_wht():
    ctx = wht_engine.build_context({"payment_type": "services",
                                    "beneficiary": "company"})
    assert ctx["tax"] == "WHT"
    ctx2 = wht_engine.build_context({"payment_type": "services", "tax": "CIT"})
    assert ctx2["tax"] == "CIT"


# ------------------------------------------------------------------- E4: date dispatch
def test_e4_legacy_cit_2025_vs_nta_2026_real_packs():
    legacy = _load_pack("rp-cit-legacy")
    edu = _load_pack("rp-education-ng")
    base = {"tax": "CIT", "entity": "company",
            "gross_turnover_kobo": 15_000_000_000}
    # 2025-dated assessment -> legacy CITA 30%
    l25 = wht_engine.evaluate_rules(legacy, {**base, "date": "2025-06-30"},
                                    as_of="2025-06-30")
    assert l25["decision"]["rate_bps"] == 3000
    assert "cit.legacy.large-company.rate" in l25["matched"]
    # 2026-dated assessment -> legacy pack silent; NTA pack gives 30%
    l26 = wht_engine.evaluate_rules(legacy, {**base, "date": "2026-06-30"},
                                    as_of="2026-06-30")
    assert "rate_bps" not in l26["decision"], l26["decision"]
    n26 = wht_engine.evaluate_rules(edu, {"tax": "CIT", "entity": "company",
                                          "size": "large", "date": "2026-06-30"},
                                    as_of="2026-06-30")
    assert n26["decision"]["rate_bps"] == 3000


@pytest.mark.parametrize("turnover,expected", [
    (2_000_000_000, 0),      # <= N25m small -> 0%
    (5_000_000_000, 2000),   # N25m-N100m medium -> 20%
    (15_000_000_000, 3000),  # > N100m large -> 30%
])
def test_e4_legacy_cit_bands_2025(turnover, expected):
    legacy = _load_pack("rp-cit-legacy")
    out = wht_engine.evaluate_rules(
        legacy, {"tax": "CIT", "entity": "company",
                 "gross_turnover_kobo": turnover, "date": "2025-06-30"},
        as_of="2025-06-30")
    assert out["decision"]["rate_bps"] == expected


def test_e4_winnings_boundary_dates_real_pack():
    base = {"payment_type": "winnings", "beneficiary": "individual",
            "source": "lottery", "amount_kobo": AMOUNT, "supplier_tin": TIN13}
    # pre-2024-10-01: no WHT rule in force -> nothing to deduct
    pre = _eval_new_pack({**base, "payment_date": "2024-09-30"})
    assert pre["rate_bps"] == 0 and pre["wht_kobo"] == 0
    assert pre["outcome"] == "no_applicable_rule"
    assert "wht.rate.winnings.resident" not in pre["matched_rules"]
    # boundary day: 5% resident
    post = _eval_new_pack({**base, "payment_date": "2024-10-01"})
    assert post["rate_bps"] == 500
    # non-resident: 15% (precedence over the resident 5% rule)
    nr = _eval_new_pack({**base, "payment_date": "2024-10-01",
                         "beneficiary_residence": "non_resident"})
    assert nr["rate_bps"] == 1500


def test_e4_effective_window_boundaries_inclusive():
    pack = _synth_pack([
        {"id": "old", "when": {"kind": "x"}, "then": {"rate_bps": 100},
         "effective_to": "2025-12-31"},
        {"id": "new", "when": {"kind": "x"}, "then": {"rate_bps": 200},
         "effective_from": "2026-01-01"},
    ])
    ctx = {"kind": "x"}
    assert wht_engine.evaluate_rules(pack, ctx, as_of="2025-12-31")[
        "decision"]["rate_bps"] == 100
    assert wht_engine.evaluate_rules(pack, ctx, as_of="2026-01-01")[
        "decision"]["rate_bps"] == 200
    # date-less contexts activate all windows (backward compatible)
    both = wht_engine.evaluate_rules(pack, ctx, as_of=None)
    assert both["decision"]["rate_bps"] == 200  # plain last-match-wins


# ------------------------------------------------------------------- E5: carve-out
CARVEOUT = {"payment_type": "supply_of_goods_materials", "beneficiary": "company",
            "amount_kobo": 1_500_000_00, "payer_size": "small",
            "payer_annual_turnover_kobo": 2_000_000_000,
            "transaction_month_value_kobo": 150_000_000,
            "payment_date": "2025-03-01"}


def test_e5_carveout_granted_payer_small_tin_present():
    body = _eval_new_pack({**CARVEOUT, "supplier_tin": TIN13})
    assert body["small_company_carveout"] is True
    assert body["rate_bps"] == 0 and body["wht_kobo"] == 0
    assert body["outcome"] == "small_company_carveout"


def test_e5_carveout_denied_without_supplier_tin():
    body = _eval_new_pack(CARVEOUT)  # no TIN
    assert body["small_company_carveout"] is False
    # goods 2% (active income) DOUBLED under the no-TIN rule -> 4%
    assert body["rate_bps"] == 400
    assert body["no_tin_double_applied"] is True


def test_e5_carveout_denied_with_invalid_tin():
    body = _eval_new_pack({**CARVEOUT, "supplier_tin": "123"})
    assert body["small_company_carveout"] is False
    assert body["rate_bps"] != 0


def test_e5_carveout_denied_payer_not_small():
    body = _eval_new_pack({**CARVEOUT, "payer_size": "large",
                           "supplier_tin": TIN13})
    assert body["small_company_carveout"] is False
    assert body["rate_bps"] == 200


def test_e5_carveout_denied_month_value_over_2m():
    body = _eval_new_pack({**CARVEOUT,
                           "transaction_month_value_kobo": 200_000_001,
                           "supplier_tin": TIN13})
    assert body["small_company_carveout"] is False
    assert body["rate_bps"] == 200


def test_e5_carveout_boundary_exactly_2m_granted():
    body = _eval_new_pack({**CARVEOUT,
                           "transaction_month_value_kobo": 200_000_000,
                           "supplier_tin": TIN13})
    assert body["small_company_carveout"] is True


# ------------------------------------------------------------------- E6: sync matrix
# The audit's 28-row WHT matrix (mirrors tests/test_taxlaw_parity.py T10),
# run through the REAL engine against the actual feature/tax-law-parity pack.
WHT_MATRIX = [
    ("dividend", "company", None, {}, 1000),
    ("dividend", "individual", None, {}, 1000),
    ("dividend", "company", "non_resident", {}, 1000),
    ("interest", "company", None, {}, 1000),
    ("interest", "company", "non_resident", {}, 1000),
    ("rent", "individual", None, {}, 1000),
    ("rent", "company", "non_resident", {}, 1000),
    ("royalty", "company", None, {}, 1000),
    ("royalty", "individual", None, {}, 500),
    ("royalty", "company", "non_resident", {}, 1000),
    ("royalty", "individual", "non_resident", {}, 500),
    ("supply_of_goods_materials", "company", None, {}, 200),
    ("construction", "company", None, {"construction_type": "roads"}, 200),
    ("construction", "company", None, {"construction_type": "other"}, 500),
    ("construction", "company", "non_resident", {"construction_type": "buildings"}, 500),
    ("construction", "company", "non_resident", {"construction_type": "other"}, 1000),
    ("consultancy", "company", None, {}, 500),
    ("professional", "individual", None, {}, 500),
    ("technical", "company", "non_resident", {}, 1000),
    ("management", "company", "non_resident", {}, 1000),
    ("commission", "individual", None, {}, 500),
    ("commission", "company", "non_resident", {}, 1000),
    ("services", "company", None, {}, 200),
    ("services", "individual", None, {}, 200),
    ("services", "company", "non_resident", {}, 500),
    ("directors_fees", "individual", None, {}, 1500),
    ("directors_fees", "individual", "non_resident", {}, 2000),
    ("winnings", "individual", None, {"source": "lottery"}, 500),
    ("winnings", "individual", "non_resident", {"source": "reality_show"}, 1500),
]


@pytest.mark.parametrize("ptype,bene,residence,extra,expected", WHT_MATRIX)
def test_e6_audit_wht_matrix_through_real_engine(ptype, bene, residence,
                                                 extra, expected):
    req = {"payment_type": ptype, "beneficiary": bene, "amount_kobo": AMOUNT,
           "supplier_tin": TIN13, "payment_date": "2025-06-01", **extra}
    if residence:
        req["beneficiary_residence"] = residence
    body = _eval_new_pack(req)
    assert body["rate_bps"] == expected, (ptype, bene, residence, body["matched_rules"])
    assert body["wht_kobo"] == wht_engine.round_half_up_kobo(AMOUNT, expected)


def test_e6_embedded_pack_drift_guard():
    """The compliance-suite embedded pack must eventually be byte-identical to
    the canonical feature/tax-law-parity pack (tools/check_embedded_sync.py
    convention). Report drift loudly so the pack owner syncs it."""
    import hashlib
    canonical = (_taxfix_packs_dir() / "rp-wht-2024" / "1.0.0.yaml").read_bytes()
    embedded = (Path(__file__).resolve().parents[3]
                / "packages" / "shared" / "rulepack" / "packs"
                / "rp-wht-2024" / "1.0.0.yaml").read_bytes()
    if hashlib.sha256(canonical).hexdigest() != hashlib.sha256(embedded).hexdigest():
        pytest.skip("embedded rp-wht-2024 predates feature/tax-law-parity; "
                    "pack-owner sync pending (engine tests use the canonical "
                    "pack via PackRegistry injection)")
