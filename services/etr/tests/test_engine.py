"""Engine tests: scope, dual-basis, SBIE, ETR, top-up, QDMTT, IIR, CFC, trace."""
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from app.engine import compute
from app.models import ComputeRequest, ConstituentEntity, Group
from app.packs import PackSet


def make_packset() -> PackSet:
    ps = PackSet("")
    ps.load_all()
    return ps


def make_group(revenue: int = 20_000_000_000_000_000) -> Group:
    return Group(id="grp-1", name="Meridian Test MNE", consolidated_revenue_kobo=revenue)


def make_entities() -> list[ConstituentEntity]:
    return [
        ConstituentEntity(id="upe", group_id="grp-1", name="HoldCo NG", jurisdiction="NG",
                          is_upe=True, net_income_kobo=500_000_000_00, covered_taxes_kobo=150_000_000_00,
                          payroll_kobo=100_000_000_00, tangible_assets_kobo=200_000_000_00),
        # low-tax subsidiary: 10% ETR -> 5% top-up
        ConstituentEntity(id="sub-mu", group_id="grp-1", name="Mauritius Sub", jurisdiction="MU",
                          parent_id="upe", ownership_bps=10000,
                          net_income_kobo=1_000_000_000_00, covered_taxes_kobo=100_000_000_00,
                          payroll_kobo=50_000_000_00, tangible_assets_kobo=50_000_000_00),
        # POPE: 60% owned by UPE
        ConstituentEntity(id="pope-ie", group_id="grp-1", name="Ireland POPE", jurisdiction="IE",
                          parent_id="upe", ownership_bps=6000, is_pope=True,
                          net_income_kobo=200_000_000_00, covered_taxes_kobo=25_000_000_00,
                          payroll_kobo=10_000_000_00, tangible_assets_kobo=10_000_000_00),
        # CFC in low-tax jurisdiction owned by UPE
        ConstituentEntity(id="cfc-gg", group_id="grp-1", name="Guernsey CFC", jurisdiction="GG",
                          parent_id="upe", ownership_bps=10000, is_cfc=True,
                          cfc_taxes_kobo=30_000_000_00,
                          net_income_kobo=300_000_000_00, covered_taxes_kobo=0,
                          payroll_kobo=0, tangible_assets_kobo=0),
    ]


def test_scope_exclusion():
    ps = make_packset()
    comp = compute(ps, make_group(revenue=100), make_entities(), ComputeRequest(group_id="grp-1", fiscal_year=2025))
    assert comp.in_scope is False
    assert comp.total_topup_kobo == 0
    assert comp.trace[0].name == "scope-check"


def test_packs_loaded():
    ps = make_packset()
    assert set(ps.packs) == {"rp-etr-nta", "rp-etr-scope", "rp-etr-cfc", "rp-globe-oecd", "rp-gir-schema"}
    assert ps.minimum_rate_bps() == 1500
    payroll_bps, assets_bps = ps.sbie_bps(2026)
    assert (payroll_bps, assets_bps) == (940, 740)


def test_etr_and_topup_mu():
    ps = make_packset()
    comp = compute(ps, make_group(), make_entities(), ComputeRequest(group_id="grp-1", fiscal_year=2025, basis="nta"))
    mu = next(j for j in comp.jurisdictions if j.jurisdiction == "MU")
    # income 1000_000_000_00, taxes 100_000_000_00 -> ETR 1000bps -> topup 500bps
    assert mu.etr_bps == 1000
    assert mu.topup_pct_bps == 500
    # SBIE 2025: 9.6% payroll + 7.6% assets of 50m each = 4_800_000_00 + 3_800_000_00
    assert mu.sbie_kobo == 480_000_000 + 380_000_000
    excess = 1_000_000_000_00 - mu.sbie_kobo
    assert mu.excess_profit_kobo == excess
    assert mu.topup_kobo == excess * 500 // 10000


def test_qdmtt_upgrade_flag():
    ps = make_packset()
    # NG UPE: income 500m*100, taxes 150m*100 -> 30% ETR, no top-up anyway.
    # Force NG low-tax by using a single low-tax NG entity:
    ents = [ConstituentEntity(id="ng-low", group_id="grp-1", name="NG LowTax", jurisdiction="NG",
                              net_income_kobo=1_000_000_000_00, covered_taxes_kobo=50_000_000_00)]
    comp_off = compute(ps, make_group(), ents, ComputeRequest(group_id="grp-1", fiscal_year=2025, qdmtt_upgrade=False))
    ng = comp_off.jurisdictions[0]
    assert ng.topup_kobo > 0 and not ng.qdmtt_applied and ng.residual_topup_kobo == ng.topup_kobo
    comp_on = compute(ps, make_group(), ents, ComputeRequest(group_id="grp-1", fiscal_year=2025, qdmtt_upgrade=True))
    ng2 = comp_on.jurisdictions[0]
    assert ng2.qdmtt_applied and ng2.qdmtt_kobo == ng2.topup_kobo and ng2.residual_topup_kobo == 0


def test_iir_allocation_upe_and_pope():
    ps = make_packset()
    comp = compute(ps, make_group(), make_entities(), ComputeRequest(group_id="grp-1", fiscal_year=2025, basis="nta"))
    # MU top-up should be allocated to UPE (iir_upe)
    mu_allocs = [a for a in comp.iir_allocations if a.from_jurisdiction == "MU"]
    assert mu_allocs and all(a.mechanism == "iir_upe" and a.to_entity_id == "upe" for a in mu_allocs)
    # IE (POPE jurisdiction, ETR 12.5%) -> pope-ie gets its own IIR first
    ie_allocs = [a for a in comp.iir_allocations if a.from_jurisdiction == "IE"]
    if ie_allocs:
        assert any(a.mechanism == "iir_pope" and a.to_entity_id == "pope-ie" for a in ie_allocs)


def test_cfc_pushdown_pool():
    ps = make_packset()
    comp = compute(ps, make_group(), make_entities(), ComputeRequest(group_id="grp-1", fiscal_year=2025, basis="globe"))
    assert comp.cfc_pool_kobo == 30_000_000_00  # 100% ownership pushdown
    gg = next(j for j in comp.jurisdictions if j.jurisdiction == "GG")
    assert gg.covered_taxes_kobo == 30_000_000_00  # 0 + pushdown
    # GG ETR = 30m/300m = 10% -> top-up 5%
    assert gg.etr_bps == 1000
    assert gg.topup_pct_bps == 500


def test_dual_basis_adjustments():
    ps = make_packset()
    ents = [ConstituentEntity(id="e1", group_id="grp-1", name="E1", jurisdiction="NG",
                              net_income_kobo=1_000_000_000_00, covered_taxes_kobo=100_000_000_00,
                              fines_penalties_kobo=10_000_000_00, development_levy_kobo=20_000_000_00,
                              stock_comp_kobo=5_000_000_00, excluded_dividends_kobo=3_000_000_00)]
    comp = compute(ps, make_group(), ents, ComputeRequest(group_id="grp-1", fiscal_year=2025, basis="dual"))
    step = next(s for s in comp.trace if s.name == "dual-basis-adjustments")
    nta = step.outputs["adjusted"]["nta"]["e1"]
    globe = step.outputs["adjusted"]["globe"]["e1"]
    # NTA: income +10m fines; taxes +20m dev levy
    assert nta["income"] == 1_000_000_000_00 + 10_000_000_00
    assert nta["taxes"] == 100_000_000_00 + 20_000_000_00
    # GloBE: income +5m stock comp -3m dividends; taxes unchanged
    assert globe["income"] == 1_000_000_000_00 + 5_000_000_00 - 3_000_000_00
    assert globe["taxes"] == 100_000_000_00


def test_audit_trace_and_digest():
    ps = make_packset()
    comp = compute(ps, make_group(), make_entities(), ComputeRequest(group_id="grp-1", fiscal_year=2025))
    names = [s.name for s in comp.trace]
    assert names == ["scope-check", "excluded-entities", "dual-basis-adjustments",
                     "cfc-pushdown"] + ["jurisdiction-etr"] * 4 + ["qdmtt", "iir-allocation", "digest"]
    assert comp.digest and len(comp.digest) == 64
    for s in comp.trace:
        assert s.rule_ref and s.pack and s.narrate


def test_loss_jurisdiction_no_topup():
    ps = make_packset()
    ents = [ConstituentEntity(id="loss", group_id="grp-1", name="Loss Co", jurisdiction="KY",
                              net_income_kobo=-50_000_000_00, covered_taxes_kobo=1_000_000_00)]
    comp = compute(ps, make_group(), ents, ComputeRequest(group_id="grp-1", fiscal_year=2025))
    assert comp.jurisdictions[0].topup_kobo == 0
    assert comp.total_topup_kobo == 0
