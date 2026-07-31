"""F3 — CIT computation chain (annual self-assessment return).

Chain: assessable profit -> capital allowance (rates as data) -> loss
relief (4-yr carryforward pre-NTA rule, effective-dated; unlimited from NTA
2025 commencement, resolved by the year the loss AROSE) -> total profit ->
CIT at tiered rate -> minimum-tax floor comparison (small co exempt) ->
development levy 4% of assessable profit -> effective tax payable.

Pillar Two / GloBE top-up (etr service) applies AFTER this CIT figure:
callers pass the domestic `effective_tax_payable_kobo` to etr; the top-up
is additive on top and never reduces it.

Deadline: within 6 months of financial year end (CITA s.55 / NTAA s.13;
rp-fmt-federal fmt.federal.cit-annual). Integer kobo, round-half-up.

REAL: full computation chain. Rates: rules_data (pack-sync comment there).
"""
from __future__ import annotations

from datetime import date
from decimal import Decimal

from .rules_data import (CAPITAL_ALLOWANCE_RATES_BPS, CIT_RATE_BPS,
                         CIT_SMALL_ASSET_THRESHOLD_KOBO,
                         CIT_SMALL_TURNOVER_THRESHOLD_KOBO,
                         DEV_LEVY_ASSESSABLE_PROFIT_BPS,
                         LOSS_RELIEF_MAX_CARRYFORWARD_YEARS,
                         MINIMUM_TAX_TURNOVER_BPS, resolve)
from .util import round_half_up


class CitError(ValueError):
    pass


def filing_deadline(fye: date) -> date:
    """6 calendar months after financial year end."""
    month = fye.month + 6
    year = fye.year + (month - 1) // 12
    month = (month - 1) % 12 + 1
    day = min(fye.day, [31, 29 if year % 4 == 0 and (year % 100 != 0 or year % 400 == 0)
                        else 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31][month - 1])
    return date(year, month, day)


def capital_allowance(assets: list[dict], when: date) -> dict:
    """assets: [{class, cost_kobo}]. IA + first-year AA on residue.

    Rates from CAPITAL_ALLOWANCE_RATES_BPS (data, pack-sync noted there).
    REAL: IA+AA year-one convention; subsequent-year AA schedules are the
    caller's asset register's job.
    """
    rates = resolve(CAPITAL_ALLOWANCE_RATES_BPS, when)
    total_ia = total_aa = 0
    lines = []
    for a in assets:
        cls = a.get("class", "other")
        if cls not in rates:
            raise CitError(f"unknown asset class {cls!r}")
        cost = int(a["cost_kobo"])
        ia = cost * rates[cls]["ia"] // 10_000
        aa = (cost - ia) * rates[cls]["aa"] // 10_000
        total_ia += ia
        total_aa += aa
        lines.append({"class": cls, "cost_kobo": cost,
                      "initial_allowance_kobo": ia, "annual_allowance_kobo": aa})
    return {"lines": lines, "initial_allowance_kobo": total_ia,
            "annual_allowance_kobo": total_aa,
            "total_capital_allowance_kobo": total_ia + total_aa}


def loss_relief(losses: list[dict], current_year: int, offset_cap_kobo: int) -> dict:
    """losses: [{year_incurred, amount_kobo}]. Returns relievable losses
    (capped at offset_cap_kobo = total profit before relief) and expired
    amounts under the effective-dated carryforward limit."""
    relievable = 0
    expired = 0
    lines = []
    for l in sorted(losses, key=lambda x: x["year_incurred"]):
        yr = int(l["year_incurred"])
        amt = int(l["amount_kobo"])
        max_years = resolve(LOSS_RELIEF_MAX_CARRYFORWARD_YEARS, date(yr, 1, 1))
        age = current_year - yr
        if max_years is not None and age > max_years:
            expired += amt
            lines.append({"year_incurred": yr, "amount_kobo": amt, "status": "expired"})
            continue
        used = min(amt, offset_cap_kobo - relievable)
        status = "relieved" if used == amt else ("partially_relieved" if used else "carried_forward")
        relievable += used
        lines.append({"year_incurred": yr, "amount_kobo": amt,
                      "relieved_kobo": used, "status": status})
    return {"lines": lines, "loss_relief_kobo": relievable, "expired_kobo": expired}


def compute_return(tin: str, fye: date, assessable_profit_kobo: int,
                   turnover_kobo: int, total_fixed_assets_kobo: int,
                   assets: list[dict] | None = None,
                   losses: list[dict] | None = None) -> dict:
    """Full CIT chain for the year of assessment ending `fye`."""
    year = fye.year
    assessable = int(assessable_profit_kobo)
    turnover = int(turnover_kobo)

    ca = capital_allowance(assets or [], fye)
    ca_claim = min(ca["total_capital_allowance_kobo"], max(assessable, 0))
    profit_after_ca = assessable - ca_claim

    lr = loss_relief(losses or [], year, max(profit_after_ca, 0))
    total_profit = profit_after_ca - lr["loss_relief_kobo"]

    small_threshold = resolve(CIT_SMALL_TURNOVER_THRESHOLD_KOBO, fye)
    asset_threshold = resolve(CIT_SMALL_ASSET_THRESHOLD_KOBO, fye)
    is_small = (turnover <= small_threshold
                and int(total_fixed_assets_kobo) <= asset_threshold)
    rates = resolve(CIT_RATE_BPS, fye)
    tier = "small" if is_small else "standard"
    cit_kobo = total_profit * rates[tier] // 10_000

    # minimum-tax floor: % of turnover, small companies exempt
    minimum_tax_kobo = 0
    floor_applied = False
    if not is_small:
        minimum_tax_kobo = round_half_up(
            Decimal(turnover) * Decimal(resolve(MINIMUM_TAX_TURNOVER_BPS, fye))
            / Decimal(10_000))
        if minimum_tax_kobo > cit_kobo:
            cit_kobo = minimum_tax_kobo
            floor_applied = True

    dev_levy_kobo = round_half_up(
        Decimal(assessable) * Decimal(resolve(DEV_LEVY_ASSESSABLE_PROFIT_BPS, fye))
        / Decimal(10_000)) if assessable > 0 else 0

    effective_tax = cit_kobo + dev_levy_kobo
    return {
        "form": "CIT-ANNUAL",
        "tin": tin,
        "fye": fye.isoformat(),
        "deadline": filing_deadline(fye).isoformat(),
        "assessable_profit_kobo": assessable,
        "capital_allowance": ca,
        "capital_allowance_claimed_kobo": ca_claim,
        "loss_relief": lr,
        "total_profit_kobo": total_profit,
        "company_tier": tier,
        "cit_rate_bps": rates[tier],
        "cit_kobo": cit_kobo,
        "minimum_tax": {"floor_kobo": minimum_tax_kobo, "applied": floor_applied,
                        "exempt_small_company": is_small},
        "development_levy_kobo": dev_levy_kobo,
        "development_levy_bps": resolve(DEV_LEVY_ASSESSABLE_PROFIT_BPS, fye),
        "effective_tax_payable_kobo": effective_tax,
        "pillar_two_note": ("GloBE top-up (etr service) applies AFTER this "
                            "domestic CIT figure; additive only."),
    }
