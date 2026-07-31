"""WHT Regulations 2024 evaluation engine (SPEC 3 T7).

All substantive rules come from the rp-wht-2024 pack (fetched from
rp-registry/rules-engine when configured, embedded copy otherwise). The
embedded copy is BYTE-IDENTICAL to the canonical pack in the meridian-rule-packs
repo (tools/check_embedded_sync.py guards drift in CI). Canonical vocabulary:
  * payment_type: dividend | interest | rent | royalty | supply_of_goods_materials
    | construction | consultancy | professional | technical | management
    | services | commission | directors_fees | winnings | telephone_charges
    | airline_ticket
  * deduction at the EARLIER of payment or settlement (Reg 3)
  * no-TIN double rate, NIN acceptable as identity for individuals (Reg 5)
  * <= N2,000,000/month small-company carve-out (Reg 4)
  * direct-debit / broker / manufacturer / import exemptions (Reg 4)
Legacy request aliases (goods, contract, service_fee, director_fee) are mapped
to the canonical vocabulary for backward compatibility.
Money is integer kobo only. Tax amounts use round-half-up per
rp-mbs-business-rules (mbs.vat.arithmetic round()).
"""

from __future__ import annotations

import os
from dataclasses import dataclass

import httpx

from meridian_py.rulepack import PackRegistry

TIN_GRAPH_URL = os.environ.get("TIN_GRAPH_URL", "").rstrip("/")

_registry = PackRegistry()

# Backward-compatible aliases -> canonical rp-wht-2024 payment_type vocabulary.
PAYMENT_TYPE_ALIASES = {
    "goods": "supply_of_goods_materials",
    "contract": "construction",
    "service_fee": "services",
    "director_fee": "directors_fees",
    "consultancy_fee": "consultancy",
    "professional_fee": "professional",
}

# Canonical payment types the pack rates (used for unknown-type 422 guard).
KNOWN_PAYMENT_TYPES = {
    "dividend", "interest", "rent", "royalty", "supply_of_goods_materials",
    "construction", "consultancy", "professional", "technical", "management",
    "services", "commission", "directors_fees", "winnings",
    "telephone_charges", "airline_ticket",
}


def round_half_up_kobo(amount_kobo: int, rate_bps: int) -> int:
    """amount * rate_bps / 10000 with round-half-up (pack-mandated round())."""
    return (amount_kobo * rate_bps + 5000) // 10_000


@dataclass
class TINCheck:
    tin: str
    valid: bool
    source: str   # tin-graph-api | local-validator
    detail: str = ""


def validate_tin(tin: str) -> TINCheck:
    """Vendor-master TIN validation via core tin-graph; local fallback."""
    if not tin:
        return TINCheck(tin, False, "local-validator", "empty TIN")
    if TIN_GRAPH_URL:
        try:
            resp = httpx.post(f"{TIN_GRAPH_URL}/v1/verify/tin", timeout=3.0,
                              json={"tin": tin})
            if resp.status_code == 200:
                body = resp.json()
                return TINCheck(tin, bool(body.get("valid")), "tin-graph-api",
                                body.get("detail", ""))
        except Exception:
            pass
    digits = "".join(c for c in tin if c.isdigit())
    ok = len(digits) == 13
    return TINCheck(tin, ok, "local-validator",
                    "13-digit TIN" if ok else "TIN must be 13 digits")


def canonical_payment_type(value: str) -> str:
    v = (value or "").strip()
    return PAYMENT_TYPE_ALIASES.get(v, v)


def build_context(req: dict) -> dict:
    """Flatten an evaluation request into the canonical rp-wht-2024 context."""
    payment_date = req.get("payment_date") or None
    settlement_date = req.get("settlement_date") or None
    beneficiary = req.get("beneficiary", "company")
    ctx: dict = {
        "payment_type": canonical_payment_type(req.get("payment_type", "")),
        "beneficiary": beneficiary,
        "payer": req.get("payer", "company"),
        # WHT Regs 2024: companies' WHT goes to NRS, individuals' to State IRS
        "tax_authority": "NRS" if beneficiary == "company" else "SIRS",
    }
    tin = req.get("supplier_tin") or req.get("vendor_tin") or ""
    nin = req.get("nin") or req.get("supplier_nin") or ""
    if tin:
        ctx["supplier_tin"] = tin
    if nin:
        ctx["supplier_nin"] = nin
    # Small-company carve-out needs the supplier's MONTHLY turnover and size —
    # never proxied from the single payment amount (audit fix: over-exemption).
    if req.get("supplier_monthly_turnover_kobo") is not None:
        ctx["supplier_monthly_turnover_kobo"] = int(req["supplier_monthly_turnover_kobo"])
    if req.get("supplier_size"):
        ctx["supplier_size"] = req["supplier_size"]
    if req.get("beneficiary_residence"):
        ctx["beneficiary_residence"] = req["beneficiary_residence"]
    if req.get("via_direct_debit"):
        ctx["payment_channel"] = "direct_debit"
    if req.get("via_broker"):
        ctx["payment_channel"] = "broker"
        ctx["instrument"] = req.get("instrument", "securities")
    if req.get("supplier_is_manufacturer"):
        ctx["supplier_role"] = "manufacturer_or_producer"
    if req.get("goods_imported"):
        ctx["goods_origin"] = "imported"
    if req.get("franked_investment_income"):
        ctx["franked_investment_income"] = True
    # Deduction timing: earlier of payment or settlement (Reg 3)
    if payment_date and settlement_date:
        ctx["payment_event"] = "payment" if payment_date <= settlement_date else "settlement"
    elif payment_date:
        ctx["payment_event"] = "payment"
    elif settlement_date:
        ctx["payment_event"] = "settlement"
    return ctx


def evaluate_wht(req: dict) -> dict:
    """Evaluate a deduction request end-to-end."""
    amount = int(req.get("amount_kobo") or 0)
    if amount <= 0:
        raise ValueError("amount_kobo must be > 0")
    ctx = build_context(req)
    if ctx["payment_type"] not in KNOWN_PAYMENT_TYPES:
        raise ValueError(
            f"unknown payment_type {req.get('payment_type')!r} "
            f"(canonical: {sorted(KNOWN_PAYMENT_TYPES)})")
    result = _registry.evaluate("rp-wht-2024", ctx)
    dec = result["decision"]
    matched = [t["rule_id"] for t in result["trace"] if t["matched"]]

    tin = ctx.get("supplier_tin", "")
    tin_check = validate_tin(tin) if tin else TINCheck("", False, "local-validator",
                                                       "no TIN supplied")
    identity_ok = bool(tin or (ctx["beneficiary"] == "individual" and ctx.get("supplier_nin")))

    # Decision assembly from the merged pack decision + matched rule ids.
    base_rate = int(dec.get("rate_bps") or 0)
    rate = base_rate
    carveout = "wht.small-co.carveout" in matched
    exempt = carveout or any(m.startswith("wht.exempt.") for m in matched)
    doubled = False
    if exempt:
        rate = 0
    elif dec.get("rate_multiplier_bps") == 20000 and not identity_ok:
        rate = base_rate * 2
        doubled = True
    outcome = ("small_company_carveout" if carveout else
               "exempt" if exempt else
               "deduct_no_tin_double" if doubled else "deduct")

    wht_kobo = round_half_up_kobo(amount, rate)
    # Deduction date: earlier of payment / settlement (Reg 3)
    dates = [d for d in (req.get("payment_date"), req.get("settlement_date")) if d]
    trigger_date = min(dates) if dates else ""
    trigger = ctx.get("payment_event", "")

    return {
        "pack": result["pack"], "via": result.get("via", "embedded-pack"),
        "subject_to_regazette": True,
        "amount_kobo": amount,
        "rate_bps": rate,
        "base_rate_bps": base_rate,
        "wht_kobo": wht_kobo,
        "net_payable_kobo": amount - wht_kobo,
        "outcome": outcome,
        "exempt": outcome == "exempt",
        "small_company_carveout": carveout,
        "no_tin_double_applied": doubled,
        "deduction_trigger": trigger,
        "deduction_date": trigger_date,
        "remit_deadline_day": dec.get("deadline_day_of_month"),
        "identity": {"tin_check": tin_check.__dict__,
                     "nin_accepted": (not tin) and identity_ok},
        "matched_rules": matched,
        "narration": dec.get("narrate", ""),
        "trace": result["trace"],
    }
