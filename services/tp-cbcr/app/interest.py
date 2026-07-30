"""Connected-party interest deductibility calculator (SPEC 3 T8): the
limitation parameters come from the rp-tp-2018 pack (30% of EBITDA,
5-year carryforward) — swappable per tenant pack pin."""

from __future__ import annotations

from meridian_py.rulepack import PackRegistry

from . import graph


def evaluate_tp_pack(tenant_id: str, ctx: dict) -> dict:
    """Evaluate rp-tp-2018 honouring the tenant's pinned pack version."""
    pinned = graph.get_pin(tenant_id, "rp-tp-2018")
    registry = PackRegistry()
    pack = registry.load("rp-tp-2018", pinned)
    result = registry.evaluate("rp-tp-2018", ctx, pinned)
    result["pack_ref"] = pack.ref
    result["tenant_pin"] = pinned or "latest"
    return result


def interest_deductibility(req: dict) -> dict:
    """Connected-party interest limitation per rp-tp-2018.

    req: {ebitda_kobo, interest_kobo, carryforward_in_kobo,
          carryforward_years_used, has_connected_party_debt, tenant_id}
    """
    ebitda = int(req.get("ebitda_kobo", 0))
    interest = int(req.get("interest_kobo", 0))
    cf_in = int(req.get("carryforward_in_kobo", 0))
    cf_years_used = int(req.get("carryforward_years_used", 0))
    has_cp_debt = bool(req.get("has_connected_party_debt", True))
    tenant_id = req.get("tenant_id", "")

    available = interest + cf_in
    ctx = {
        "has_connected_party_debt": has_cp_debt,
        "interest_exceeds_limit": False,  # refined after limit known
    }
    result = evaluate_tp_pack(tenant_id, ctx)
    dec = result["decision"]
    limit_bps = int(dec.get("interest_limit_bps_of_ebitda", 3000))
    max_cf_years = int(dec.get("carryforward_years", 5))

    limit = ebitda * limit_bps // 10_000 if has_cp_debt else available
    exceeds = available > limit
    # Re-evaluate with the exceeds flag so pack trace is accurate
    if exceeds:
        ctx["interest_exceeds_limit"] = True
        result = evaluate_tp_pack(tenant_id, ctx)
        dec = result["decision"]

    deductible = min(available, limit)
    disallowed = available - deductible
    # carryforward expires after max_cf_years
    cf_out = disallowed if cf_years_used < max_cf_years else 0
    expired = disallowed - cf_out
    return {
        "pack": result.get("pack_ref", result["pack"]),
        "tenant_pin": result.get("tenant_pin", "latest"),
        "has_connected_party_debt": has_cp_debt,
        "ebitda_kobo": ebitda,
        "interest_kobo": interest,
        "carryforward_in_kobo": cf_in,
        "available_interest_kobo": available,
        "limit_bps_of_ebitda": limit_bps,
        "limit_kobo": limit,
        "deductible_kobo": deductible,
        "disallowed_kobo": disallowed,
        "carryforward_out_kobo": cf_out,
        "carryforward_expired_kobo": expired,
        "max_carryforward_years": max_cf_years,
        "narration": dec.get("narrate", ""),
        "trace": result["trace"],
    }
