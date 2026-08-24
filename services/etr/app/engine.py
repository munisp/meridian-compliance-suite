"""Pillar Two ETR computation engine (T9 / wf-etr-compute).

Pipeline: scope -> dual-basis adjustments (NTA + OECD GloBE) -> jurisdictional
blending -> CFC pushdown pool -> SBIE carve-out -> ETR -> top-up % -> QDMTT ->
IIR allocation down the ownership chain. Every step emits an audit-defensible
StepTrace (rule ref + pack version + inputs/outputs).
"""
from __future__ import annotations

import hashlib
import json
from datetime import datetime, timezone
from typing import Any
from uuid import uuid4

from .models import (Computation, ComputeRequest, ConstituentEntity, Group,
                     IIRAllocation, JurisdictionResult, StepTrace)
from .packs import PackSet


def _now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


class Tracer:
    def __init__(self) -> None:
        self.steps: list[StepTrace] = []

    def step(self, name: str, rule_ref: str, pack: str, inputs: dict, outputs: dict, narrate: str) -> None:
        self.steps.append(StepTrace(
            step_no=len(self.steps) + 1, name=name, rule_ref=rule_ref, pack=pack,
            inputs=inputs, outputs=outputs, narrate=narrate))


def _adjust(entity: ConstituentEntity, basis: str, ps: PackSet) -> tuple[int, int, list[str]]:
    """Apply basis-specific adjustments. Returns (adj_income, adj_taxes, applied_rules)."""
    income = entity.net_income_kobo
    taxes = entity.covered_taxes_kobo
    applied: list[str] = []
    if basis in ("nta", "dual"):
        # rp-etr-nta adjustments
        if entity.fines_penalties_kobo:
            income += entity.fines_penalties_kobo
            applied.append("etr.nta.adjustment.disallowed_fines")
        if entity.development_levy_kobo:
            taxes += entity.development_levy_kobo
            applied.append("etr.nta.adjustment.development_levy")
        if entity.priority_credit_kobo:
            taxes -= entity.priority_credit_kobo
            applied.append("etr.nta.adjustment.priority_credit")
    if basis in ("globe", "dual"):
        # rp-globe-oecd adjustments
        if entity.stock_comp_kobo:
            income += entity.stock_comp_kobo
            applied.append("globe.adjustment.stock_comp")
        if entity.asymmetric_fx_kobo:
            income += entity.asymmetric_fx_kobo
            applied.append("globe.adjustment.asymmetric_fx")
        if entity.excluded_dividends_kobo:
            income -= entity.excluded_dividends_kobo
            applied.append("globe.adjustment.excluded_dividends")
    return income, taxes, applied


def compute(ps: PackSet, group: Group, entities: list[ConstituentEntity], req: ComputeRequest,
            qdmtt_armed: bool = False) -> Computation:
    """Run the ETR pipeline.

    `qdmtt_armed` is the SERVER-RESOLVED reg-watch gate state (see
    app.gates.qdmtt_upgrade_armed). The client-supplied
    `req.qdmtt_upgrade` field is deliberately ignored (audit fix B2-#10).
    """
    tr = Tracer()
    pack_versions = ps.versions()

    # ---- step 1: scope (rp-etr-scope) ----
    threshold = ps.revenue_threshold_kobo()
    in_scope = group.consolidated_revenue_kobo >= threshold
    tr.step("scope-check", "etr.scope.revenue_threshold", "rp-etr-scope",
            {"consolidated_revenue_kobo": group.consolidated_revenue_kobo, "threshold_ngn_kobo": threshold},
            {"in_scope": in_scope},
            "MNE group in scope when consolidated revenue >= EUR 750m equivalent")
    if not in_scope:
        return Computation(
            id=f"cmp-{uuid4().hex[:12]}", group_id=group.id, fiscal_year=req.fiscal_year,
            basis=req.basis, created_at=_now(), in_scope=False,
            scope_reason="consolidated revenue below threshold", qdmtt_upgrade=qdmtt_armed,
            jurisdictions=[], iir_allocations=[], total_topup_kobo=0, cfc_pool_kobo=0,
            trace=tr.steps, pack_versions=pack_versions)

    # ---- step 2: exclusions (rp-etr-scope excluded entities) ----
    excluded_types = set(ps.excluded_entity_types())
    in_scope_entities = [e for e in entities if e.entity_type not in excluded_types]
    tr.step("excluded-entities", "etr.scope.exclusions", "rp-etr-scope",
            {"entity_count": len(entities), "excluded_types": sorted(excluded_types)},
            {"in_scope_entities": [e.id for e in in_scope_entities]},
            "GloBE Art. 1.5 excluded entities removed from computation")

    # ---- step 3: dual-basis adjustments (rp-etr-nta + rp-globe-oecd) ----
    bases = ["nta", "globe"] if req.basis == "dual" else [req.basis]
    adjusted: dict[str, dict[str, tuple[int, int]]] = {b: {} for b in bases}
    applied_rules: dict[str, list[str]] = {}
    for e in in_scope_entities:
        for b in bases:
            inc, tax, rules = _adjust(e, b, ps)
            adjusted[b][e.id] = (inc, tax)
            applied_rules.setdefault(e.id, [])
            applied_rules[e.id].extend(r for r in rules if r not in applied_rules[e.id])
    tr.step("dual-basis-adjustments", "etr.nta.adjustment.* + globe.adjustment.*",
            "rp-etr-nta,rp-globe-oecd",
            {"basis": req.basis},
            {"adjusted": {b: {eid: {"income": v[0], "taxes": v[1]} for eid, v in m.items()}
                          for b, m in adjusted.items()}},
            "NTA basis: dev levy +, fines +, priority credit -; GloBE basis: stock comp +, asym FX +, excluded dividends -")

    # ---- step 4: jurisdictional blending + CFC pushdown (rp-etr-cfc) ----
    jurisdictions: dict[str, list[ConstituentEntity]] = {}
    for e in in_scope_entities:
        jurisdictions.setdefault(e.jurisdiction.upper(), []).append(e)

    cfc_pool_total = 0
    cfc_pushdown: dict[str, int] = {}  # jurisdiction -> pushed-down covered taxes
    for e in in_scope_entities:
        if e.is_cfc and e.cfc_taxes_kobo > 0:
            push = e.cfc_taxes_kobo * e.ownership_bps // 10000
            cfc_pushdown[e.jurisdiction.upper()] = cfc_pushdown.get(e.jurisdiction.upper(), 0) + push
            cfc_pool_total += push
    tr.step("cfc-pushdown", "etr.cfc.pushdown + etr.cfc.pool", "rp-etr-cfc",
            {"cfc_entities": [e.id for e in in_scope_entities if e.is_cfc]},
            {"pushdown_by_jurisdiction_kobo": cfc_pushdown, "cfc_pool_total_kobo": cfc_pool_total},
            "CFC-level covered taxes allocated to CFC jurisdiction pools by ownership %")

    # ---- step 5: SBIE + ETR + top-up per jurisdiction per basis ----
    payroll_bps, assets_bps = ps.sbie_bps(req.fiscal_year)
    min_rate = ps.minimum_rate_bps()
    results: list[JurisdictionResult] = []
    for juris, ents in sorted(jurisdictions.items()):
        payroll = sum(e.payroll_kobo for e in ents)
        assets = sum(e.tangible_assets_kobo for e in ents)
        sbie = payroll * payroll_bps // 10000 + assets * assets_bps // 10000
        best: JurisdictionResult | None = None
        for b in bases:
            income = sum(adjusted[b][e.id][0] for e in ents)
            taxes = sum(adjusted[b][e.id][1] for e in ents) + cfc_pushdown.get(juris, 0)
            if income <= 0:
                jr = JurisdictionResult(
                    jurisdiction=juris, basis=b, entities=[e.id for e in ents],
                    net_income_kobo=income, covered_taxes_kobo=taxes, sbie_kobo=sbie,
                    excess_profit_kobo=0, etr_bps=0, topup_pct_bps=0, topup_kobo=0,
                    qdmtt_applied=False, qdmtt_kobo=0, residual_topup_kobo=0)
            else:
                etr_bps = taxes * 10000 // income
                topup_pct = max(0, min_rate - etr_bps)
                excess = max(0, income - sbie)
                topup = excess * topup_pct // 10000
                jr = JurisdictionResult(
                    jurisdiction=juris, basis=b, entities=[e.id for e in ents],
                    net_income_kobo=income, covered_taxes_kobo=taxes, sbie_kobo=sbie,
                    excess_profit_kobo=excess, etr_bps=etr_bps, topup_pct_bps=topup_pct,
                    topup_kobo=topup, qdmtt_applied=False, qdmtt_kobo=0, residual_topup_kobo=topup)
            if best is None or jr.topup_kobo > best.topup_kobo:
                best = jr
        assert best is not None
        tr.step("jurisdiction-etr", "globe.minimum_rate + globe.sbie.transition", "rp-globe-oecd",
                {"jurisdiction": juris, "payroll_kobo": payroll, "assets_kobo": assets,
                 "sbie_bps": {"payroll": payroll_bps, "assets": assets_bps}},
                best.model_dump(),
                f"ETR={best.etr_bps}bps vs minimum {min_rate}bps; top-up on excess profit after SBIE")
        results.append(best)

    # ---- step 6: QDMTT (rp-etr-nta etr.nta.qdmtt + upgrade flag) ----
    qdmtt_juris = set(ps.qdmtt_jurisdictions())
    for jr in results:
        if jr.topup_kobo > 0 and jr.jurisdiction in qdmtt_juris and qdmtt_armed:
            jr.qdmtt_applied = True
            jr.qdmtt_kobo = jr.topup_kobo
            jr.residual_topup_kobo = 0
    tr.step("qdmtt", "etr.nta.qdmtt", "rp-etr-nta",
            {"qdmtt_jurisdictions": sorted(qdmtt_juris), "qdmtt_upgrade": qdmtt_armed},
            {"applied": [jr.jurisdiction for jr in results if jr.qdmtt_applied]},
            "QDMTT collects domestic top-up locally when the qdmtt_upgrade gate is armed")

    # ---- step 7: IIR allocation down the ownership chain (rp-globe-oecd iir.ordering) ----
    by_id = {e.id: e for e in in_scope_entities}
    allocations: list[IIRAllocation] = []
    for jr in results:
        if jr.residual_topup_kobo <= 0 or jr.excess_profit_kobo <= 0:
            continue
        for e in by_id.values():
            if e.jurisdiction.upper() != jr.jurisdiction:
                continue
            ent_excess = max(0, adjusted[jr.basis][e.id][0] - (
                e.payroll_kobo * payroll_bps // 10000 + e.tangible_assets_kobo * assets_bps // 10000))
            if ent_excess <= 0:
                continue
            share = jr.residual_topup_kobo * ent_excess // jr.excess_profit_kobo
            if share <= 0:
                continue
            chain: list[ConstituentEntity] = []
            cur = e
            while cur.parent_id and cur.parent_id in by_id:
                parent = by_id[cur.parent_id]
                chain.append(parent)
                cur = parent
            if e.is_pope:
                # A POPE applies the IIR to its own low-taxed income first
                # (GloBE Art 2.1.4) before any top-down allocation to the UPE.
                allocations.append(IIRAllocation(
                    from_jurisdiction=jr.jurisdiction, to_entity_id=e.id,
                    to_entity_name=e.name, mechanism="iir_pope",
                    amount_kobo=share, ownership_bps=10000))
                continue
            pope = next((p for p in chain if p.is_pope), None)
            target = pope or (chain[-1] if chain else None)
            if target is None:
                continue
            # effective ownership of target down to e (product of chain bps)
            eff = 10000
            node = target
            path: list[ConstituentEntity] = []
            walk = e
            while walk is not node and walk.parent_id and walk.parent_id in by_id:
                path.append(walk)
                walk = by_id[walk.parent_id]
            for w in path:
                eff = eff * w.ownership_bps // 10000
            amount = share * eff // 10000 if pope else share
            allocations.append(IIRAllocation(
                from_jurisdiction=jr.jurisdiction, to_entity_id=target.id,
                to_entity_name=target.name,
                mechanism="iir_pope" if pope else "iir_upe",
                amount_kobo=amount, ownership_bps=eff))
            if pope and chain:
                remainder = share - amount
                if remainder > 0:
                    upe = chain[-1]
                    allocations.append(IIRAllocation(
                        from_jurisdiction=jr.jurisdiction, to_entity_id=upe.id,
                        to_entity_name=upe.name, mechanism="iir_upe",
                        amount_kobo=remainder, ownership_bps=10000))
    tr.step("iir-allocation", "globe.iir.ordering", "rp-globe-oecd",
            {"pope_threshold_bps": ps.pope_threshold_bps()},
            {"allocations": [a.model_dump() for a in allocations]},
            "Residual top-up allocated top-down: POPE (ownership < 80%) applies IIR before UPE")

    total_topup = sum(jr.topup_kobo for jr in results)
    digest = hashlib.sha256(json.dumps(
        {"jurisdictions": [jr.model_dump() for jr in results],
         "allocations": [a.model_dump() for a in allocations]},
        sort_keys=True).encode()).hexdigest()
    tr.step("digest", "filingpack.contents", "rp-gir-schema",
            {}, {"sha256": digest}, "Computation digest seals the audit trail")

    return Computation(
        id=f"cmp-{uuid4().hex[:12]}", group_id=group.id, fiscal_year=req.fiscal_year,
        basis=req.basis, created_at=_now(), in_scope=True,
        scope_reason="consolidated revenue >= threshold", qdmtt_upgrade=qdmtt_armed,
        jurisdictions=results, iir_allocations=allocations,
        total_topup_kobo=total_topup, cfc_pool_kobo=cfc_pool_total,
        trace=tr.steps, pack_versions=pack_versions, digest=digest)
