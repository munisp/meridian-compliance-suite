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

New pack semantics (meridian-rule-packs feature/tax-law-parity, audit
2026-07-31 findings T3/T4/T7 + gap #14) honored by evaluate_rules():
  * precedence (rule-level or then.precedence): higher precedence wins on
    multi-match — a generic non-resident rule can no longer clobber the
    royalty non-resident-individual 5% rate (finding #7);
  * `not_in` operator in when-conditions (map form and key__not_in suffix) —
    scopes the no-TIN double rate to active income (finding #9);
  * `tax:` when-discriminator — CIT / Development Levy / TET-style rules only
    fire for their own tax and cannot clobber each other's computed fields;
  * date-aware rule dispatch: rule effective_from/effective_to are matched
    against the transaction date (earlier of payment/settlement), fixing the
    date-blind engine (gap #14) — 2025-dated facts hit legacy rules,
    2026-dated hit NTA rules, winnings are unrated before 2024-10-01 and
    5%/15% from 2024-10-01.
Small-company carve-out (finding #8) is enforced engine-side: it is NEVER
granted without a present, valid supplier TIN; the payer-small and
<= N2m/month conditions are evaluated as pack when-facts.
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from typing import Any

import httpx

from meridian_py.rulepack import Pack, PackRegistry
from meridian_py.rulepack import _match_cond as _shared_match_cond

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


# ------------------------------------------------------------------ new pack semantics
# Mirrors the reference matcher in meridian-rule-packs tests/test_taxlaw_parity.py
# (branch feature/tax-law-parity):
#   * `not_in` operator in `when` condition maps (and `key__not_in` suffix)
#   * rule-level `effective_from` / `effective_to` date dispatch on the
#     transaction date (gap #14: the engine was date-blind)
#   * `precedence` (rule-level or `then.precedence`): on multi-match the higher
#     precedence rule wins; ties keep pack file order (last-match-wins)
#   * `tax:` when-discriminator: rules keyed `when.tax: CIT|DEV_LEVY|TET|...`
#     only fire when the context carries that tax, so levy/CIT-style rules can
#     never clobber each other's (or WHT's) computed fields.


def _match_cond(key: str, want: Any, ctx: dict) -> tuple[bool, str]:
    """Shared matcher + `not_in` extension (map form and __not_in suffix)."""
    if key.endswith("__not_in"):
        base = key[: -len("__not_in")]
        got = ctx.get(base)
        ok = not any(str(item) == str(got) for item in (want or []))
        return ok, "" if ok else f"{base}={got} in {want}"
    if isinstance(want, dict) and "not_in" in want:
        got = ctx.get(key)
        if any(str(item) == str(got) for item in (want.get("not_in") or [])):
            return False, f"{key}={got} in {want['not_in']}"
        rest = {k: v for k, v in want.items() if k != "not_in"}
        if rest:
            return _shared_match_cond(key, rest, ctx)
        return True, ""
    return _shared_match_cond(key, want, ctx)


def _rule_active(rule: dict, as_of: str | None) -> bool:
    """Rule-level effective-date dispatch (ISO dates compare lexicographically).
    Rules without windows are always active; a request without a date activates
    all windows (backward compatible)."""
    if not as_of:
        return True
    ef, et = rule.get("effective_from"), rule.get("effective_to")
    if ef and as_of < str(ef):
        return False
    if et and as_of > str(et):
        return False
    return True


def _rule_precedence(rule: dict) -> int:
    if rule.get("precedence") is not None:
        return int(rule["precedence"])
    return int((rule.get("then") or {}).get("precedence", 0) or 0)


def evaluate_rules(pack: Pack, ctx: dict, as_of: str | None = None) -> dict:
    """Evaluate pack rules over ctx with date dispatch + precedence merge.

    Matched rules are applied in ascending (precedence, file order), so the
    highest-precedence matching rule wins each decision field on multi-match.
    """
    scored: list[tuple[int, int, dict]] = []
    trace: list[dict] = []
    for idx, rule in enumerate(pack.rules):
        rid = rule.get("id")
        prec = _rule_precedence(rule)
        if not _rule_active(rule, as_of):
            trace.append({"rule_id": rid, "matched": False, "precedence": prec,
                          "reason": f"inactive at {as_of} (effective "
                                    f"{rule.get('effective_from')}.."
                                    f"{rule.get('effective_to')})",
                          "skipped": "effective-window"})
            continue
        matched, why = True, ""
        for k, want in (rule.get("when") or {}).items():
            ok, why = _match_cond(k, want, ctx)
            if not ok:
                matched = False
                break
        trace.append({"rule_id": rid, "matched": matched, "precedence": prec,
                      "reason": "" if matched else why})
        if matched:
            scored.append((prec, idx, rule))
    scored.sort(key=lambda t: (t[0], t[1]))
    decision: dict[str, Any] = {}
    for _, _, rule in scored:
        decision.update({k: v for k, v in (rule.get("then") or {}).items()
                         if k != "precedence"})
    return {"pack": pack.ref, "decision": decision, "trace": trace,
            "matched": [r.get("id") for _, _, r in scored]}


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
    # Small-company carve-out (WHT Regs 2024 reg. 4, canonical pack): keyed on
    # the PAYER being a small company (<= N25m p.a.), the transaction value not
    # exceeding N2m in the calendar month, AND the supplier having a valid TIN.
    # Legacy supplier-side facts (supplier_size/supplier_monthly_turnover_kobo)
    # are intentionally NOT honoured alone — that modeling was the audit's
    # over-exemption bug.
    if req.get("payer_size"):
        ctx["payer_size"] = req["payer_size"]
    elif req.get("payer_is_small_company"):
        ctx["payer_size"] = "small"
    if req.get("payer_annual_turnover_kobo") is not None:
        ctx["payer_annual_turnover_kobo"] = int(req["payer_annual_turnover_kobo"])
    if req.get("transaction_month_value_kobo") is not None:
        ctx["transaction_month_value_kobo"] = int(req["transaction_month_value_kobo"])
    # Legacy supplier-side facts are still forwarded (older pack versions keyed
    # the carve-out on them) but the engine gate no longer honours them alone.
    if req.get("supplier_monthly_turnover_kobo") is not None:
        ctx["supplier_monthly_turnover_kobo"] = int(req["supplier_monthly_turnover_kobo"])
    if req.get("supplier_size"):
        ctx["supplier_size"] = req["supplier_size"]
    if req.get("transaction_month_value_kobo") is None and req.get("amount_kobo") is not None:
        # Default: this payment is the transaction value in the month.
        ctx["transaction_month_value_kobo"] = int(req["amount_kobo"])
    # Construction rate split (First Schedule): roads/bridges/buildings/power
    # plants vs any other construction — default to "other" (the general rate).
    if req.get("construction_type"):
        ctx["construction_type"] = req["construction_type"]
    elif ctx["payment_type"] == "construction":
        ctx["construction_type"] = "other"
    # Winnings source (lottery/gaming/reality_show) for the First Schedule rates.
    if req.get("source"):
        ctx["source"] = req["source"]
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
    if req.get("source"):
        ctx["source"] = req["source"]  # winnings: lottery | gaming | reality_show
    if req.get("construction_type"):
        ctx["construction_type"] = req["construction_type"]
    # `tax` when-discriminator (E3): keeps CIT/Dev-Levy/TET-style rules from
    # clobbering each other's computed fields; WHT requests default to WHT.
    ctx["tax"] = req.get("tax") or "WHT"
    # Deduction timing: earlier of payment or settlement (Reg 3)
    if payment_date and settlement_date:
        ctx["payment_event"] = "payment" if payment_date <= settlement_date else "settlement"
    elif payment_date:
        ctx["payment_event"] = "payment"
    elif settlement_date:
        ctx["payment_event"] = "settlement"
    # Transaction date fact: drives rule effective-window dispatch (E4) and
    # `when.date` conditions (e.g. rp-cit-legacy date-gated legacy rates).
    dates = [d for d in (payment_date, settlement_date,
                       req.get("date") or req.get("transaction_date")) if d]
    if dates:
        ctx["date"] = min(dates)
    return ctx


def evaluate_wht(req: dict, pack: Pack | None = None,
                 registry: PackRegistry | None = None) -> dict:
    """Evaluate a deduction request end-to-end.

    `pack`/`registry` are injectable for tests (e.g. loading the canonical
    packs from the meridian-rule-packs checkout via RP_PACKS_DIR).
    """
    amount = int(req.get("amount_kobo") or 0)
    if amount <= 0:
        raise ValueError("amount_kobo must be > 0")
    ctx = build_context(req)
    if ctx["payment_type"] not in KNOWN_PAYMENT_TYPES:
        raise ValueError(
            f"unknown payment_type {req.get('payment_type')!r} "
            f"(canonical: {sorted(KNOWN_PAYMENT_TYPES)})")
    as_of = ctx.get("date")  # transaction date -> rule effective-window dispatch
    via = "embedded-pack"
    if pack is None and registry is None and os.environ.get("RULES_ENGINE_URL"):
        # Deployment path: core rules-engine (Go) evaluates remotely.
        result = (registry or _registry).evaluate("rp-wht-2024", ctx)
        via = result.get("via", "rules-engine")
        matched = [t["rule_id"] for t in result["trace"] if t.get("matched")]
    else:
        pack = pack or (registry or _registry).load("rp-wht-2024")
        tin0 = ctx.get("supplier_tin", "")
        tin0_valid = validate_tin(tin0).valid if tin0 else False
        rules = pack.rules
        if not tin0_valid and any(r.get("id") == "wht.small-co.carveout"
                                  for r in rules):
            # E5 engine-side enforcement (audit finding #8): the small-company
            # carve-out is NEVER granted without a present, valid supplier TIN —
            # drop the rule before matching so the normal rate applies.
            rules = [r for r in rules if r.get("id") != "wht.small-co.carveout"]
        if rules is not pack.rules:
            pack = Pack(id=pack.id, version=pack.version,
                        effective_from=pack.effective_from,
                        effective_to=pack.effective_to, status=pack.status,
                        subject_to_regazette=pack.subject_to_regazette,
                        provenance=pack.provenance, signed=pack.signed,
                        rules=rules, raw=pack.raw)
        result = evaluate_rules(pack, ctx, as_of=as_of)
        matched = result["matched"]
    dec = result["decision"]

    tin = ctx.get("supplier_tin", "")
    tin_check = validate_tin(tin) if tin else TINCheck("", False, "local-validator",
                                                       "no TIN supplied")
    identity_ok = bool(tin or (ctx["beneficiary"] == "individual" and ctx.get("supplier_nin")))

    # Decision assembly from the merged pack decision + matched rule ids.
    rate_rule_matched = "rate_bps" in dec
    base_rate = int(dec.get("rate_bps") or 0)
    rate = base_rate
    # The carve-out rule only survives evaluation with a valid TIN (E5).
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
               "deduct_no_tin_double" if doubled else
               "deduct" if rate_rule_matched else "no_applicable_rule")

    wht_kobo = round_half_up_kobo(amount, rate)
    # Deduction date: earlier of payment / settlement (Reg 3)
    dates = [d for d in (req.get("payment_date"), req.get("settlement_date")) if d]
    trigger_date = min(dates) if dates else (ctx.get("date") or "")
    trigger = ctx.get("payment_event", "")

    return {
        "pack": result["pack"], "via": result.get("via", "embedded-pack"),
        "subject_to_regazette": True,
        "as_of_date": as_of or "",
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
