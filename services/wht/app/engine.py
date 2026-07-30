"""WHT Regulations 2024 evaluation engine (SPEC 3 T7).

All substantive rules come from the rp-wht-2024 pack (fetched from
rp-registry/rules-engine when configured, embedded copy otherwise):
  * deduction at the EARLIER of payment or settlement (Reg 3)
  * no-TIN double rate, NIN acceptable as identity for individuals (Reg 8)
  * <= N2,000,000/month small-company carve-out for valid-TIN suppliers (Reg 5)
  * direct-debit / broker / manufacturer / import exemptions (Reg 4)
Money is integer kobo only.
"""

from __future__ import annotations

import os
from dataclasses import dataclass

import httpx

from meridian_py.rulepack import PackRegistry

TIN_GRAPH_URL = os.environ.get("TIN_GRAPH_URL", "").rstrip("/")

_registry = PackRegistry()


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


def build_context(req: dict) -> dict:
    """Flatten an evaluation request into the rp-wht-2024 context."""
    payment_date = req.get("payment_date") or None
    settlement_date = req.get("settlement_date") or None
    ctx: dict = {
        "payment_type": req.get("payment_type", ""),
        "beneficiary": req.get("beneficiary", "company"),
        "has_tin": bool(req.get("supplier_tin")),
        "has_nin": bool(req.get("nin")),
        "monthly_amount_kobo": int(req.get("monthly_amount_kobo")
                                   or req.get("amount_kobo") or 0),
        "via_direct_debit": bool(req.get("via_direct_debit")),
        "via_broker": bool(req.get("via_broker")),
        "supplier_is_manufacturer": bool(req.get("supplier_is_manufacturer")),
        "goods_imported": bool(req.get("goods_imported")),
    }
    if payment_date:
        ctx["payment_date"] = payment_date
    if settlement_date:
        ctx["settlement_date"] = settlement_date
    if payment_date and settlement_date:
        ctx["payment_before_settlement"] = payment_date <= settlement_date
    # identity_ok: valid TIN, or NIN acceptable for individuals (Reg 8(4))
    ctx["identity_ok"] = bool(
        ctx["has_tin"] or (ctx["beneficiary"] == "individual" and ctx["has_nin"]))
    return ctx


def evaluate_wht(req: dict) -> dict:
    """Evaluate a deduction request end-to-end."""
    amount = int(req.get("amount_kobo") or 0)
    if amount <= 0:
        raise ValueError("amount_kobo must be > 0")
    ctx = build_context(req)
    result = _registry.evaluate("rp-wht-2024", ctx)
    dec = result["decision"]

    tin = req.get("supplier_tin", "")
    tin_check = validate_tin(tin) if tin else TINCheck("", False, "local-validator",
                                                       "no TIN supplied")
    # Precedence: exemptions -> small-co carve-out -> base rate (+ no-TIN x2)
    base_rate = int(dec.get("rate_bps") or 0)
    rate = base_rate
    outcome = "deduct"
    if dec.get("exempt"):
        rate, outcome = 0, "exempt"
    elif dec.get("small_company_carveout"):
        rate, outcome = 0, "small_company_carveout"
    elif dec.get("rate_multiplier") == 2 and not ctx["identity_ok"]:
        rate = base_rate * 2
        outcome = "deduct_no_tin_double"

    wht_kobo = amount * rate // 10_000
    trigger = dec.get("deduction_trigger", "")
    trigger_date = ""
    if trigger == "payment":
        trigger_date = req.get("payment_date", "")
    elif trigger == "settlement":
        trigger_date = req.get("settlement_date", "")

    narratives = [t for t in result["trace"] if t["matched"]]
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
        "small_company_carveout": outcome == "small_company_carveout",
        "no_tin_double_applied": outcome == "deduct_no_tin_double",
        "deduction_trigger": trigger,
        "deduction_date": trigger_date,
        "remit_deadline_day": dec.get("remit_deadline_day"),
        "identity": {"tin_check": tin_check.__dict__,
                     "nin_accepted": (not ctx["has_tin"]) and ctx["identity_ok"]},
        "matched_rules": [t["rule_id"] for t in narratives],
        "narration": dec.get("narrate", ""),
        "trace": result["trace"],
    }
