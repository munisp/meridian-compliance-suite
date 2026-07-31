"""F2 — PAYE monthly remittance schedule + annual Form H1.

Monthly PAYE return: employee-level rows (TIN, name, gross, pension/reliefs,
tax), due the 10th of the following month (PITA s.81; rp-fmt-federal
fmt.federal.paye). Annual Form H1 reconciliation due 31 January.

Tax computation mirrors rp-paye-pitra-legacy: exempt deductions (pension,
NHF, NHIS, life assurance, gratuity), CRA = higher of fixed/1% gross plus
20% of gross, annual bands 7-24%, 1% minimum tax. Monthly tax = annual/12
rounded half-up. Integer kobo throughout.

REAL: schedule/return generation, employer aggregation, H1.
"""
from __future__ import annotations

import itertools
from datetime import date
from decimal import Decimal

from .rules_data import (PAYE_BANDS, PAYE_CRA, PAYE_EXEMPT_DEDUCTION_KEYS,
                         PAYE_MINIMUM_TAX_BPS, resolve)
from .util import deadline_nth_of_following_month, round_half_up

PAYE_FILING_DAY = 10
H1_DEADLINE_MONTH_DAY = (1, 31)  # 31 Jan following the year of income

_ids = itertools.count(1)


class PayeError(ValueError):
    pass


def employee_annual_tax(gross_kobo: int, deductions: dict, when: date) -> dict:
    gross = int(gross_kobo)
    exempt = sum(int(deductions.get(k, 0)) for k in PAYE_EXEMPT_DEDUCTION_KEYS)
    fixed, cra_bps = resolve(PAYE_CRA, when)
    cra = max(fixed, gross // 100) + gross * cra_bps // 10_000
    taxable = max(gross - exempt - cra, 0)
    tax = 0
    remaining = taxable
    for width, bps in resolve(PAYE_BANDS, when):
        slice_kobo = remaining if width is None else min(remaining, width)
        tax += slice_kobo * bps // 10_000
        remaining -= slice_kobo
        if remaining <= 0:
            break
    min_tax = gross * resolve(PAYE_MINIMUM_TAX_BPS, when) // 10_000
    if taxable == 0:
        tax = min_tax
    return {
        "gross_annual_kobo": gross,
        "exempt_deductions_kobo": exempt,
        "cra_kobo": cra,
        "taxable_income_kobo": taxable,
        "annual_tax_kobo": tax,
        "monthly_tax_kobo": round_half_up(Decimal(tax) / Decimal(12)),
    }


def build_monthly_schedule(tin_employer: str, period: str,
                           employees: list[dict]) -> dict:
    """employees: [{tin, name, gross_kobo, pension_kobo?, nhf_kobo?, ...}].
    gross_kobo is MONTHLY gross; computation annualises."""
    year, month = None, None
    from .util import parse_period
    year, month = parse_period(period)
    when = date(year, month, 1)
    rows = []
    for e in employees:
        monthly_gross = int(e["gross_kobo"])
        deductions = {k: int(e.get(k, 0)) * 12 for k in PAYE_EXEMPT_DEDUCTION_KEYS}
        r = employee_annual_tax(monthly_gross * 12, deductions, when)
        rows.append({
            "tin": e["tin"],
            "name": e["name"],
            "gross_kobo": monthly_gross,
            "pension_kobo": int(e.get("pension_kobo", 0)),
            "reliefs_kobo": sum(int(e.get(k, 0)) for k in PAYE_EXEMPT_DEDUCTION_KEYS),
            "cra_kobo": round_half_up(Decimal(r["cra_kobo"]) / Decimal(12)),
            "tax_kobo": r["monthly_tax_kobo"],
        })
    return {
        "form": "PAYE-MONTHLY",
        "employer_tin": tin_employer,
        "period": period,
        "deadline": deadline_nth_of_following_month(period, PAYE_FILING_DAY).isoformat(),
        "rows": rows,
        "totals": {
            "employees": len(rows),
            "gross_kobo": sum(r["gross_kobo"] for r in rows),
            "tax_kobo": sum(r["tax_kobo"] for r in rows),
        },
    }


def build_form_h1(tin_employer: str, year: int,
                  monthly_schedules: list[dict]) -> dict:
    """Annual reconciliation (Form H1) aggregating the year's monthly
    schedules for the employer. Due 31 Jan of year+1."""
    per_employee: dict[str, dict] = {}
    for sched in monthly_schedules:
        if sched["employer_tin"] != tin_employer:
            raise PayeError("schedule employer mismatch in H1 aggregation")
        if not sched["period"].startswith(f"{year}-"):
            raise PayeError(f"schedule period {sched['period']} outside year {year}")
        for row in sched["rows"]:
            agg = per_employee.setdefault(row["tin"], {
                "tin": row["tin"], "name": row["name"],
                "gross_kobo": 0, "tax_kobo": 0, "months": 0})
            agg["gross_kobo"] += row["gross_kobo"]
            agg["tax_kobo"] += row["tax_kobo"]
            agg["months"] += 1
    rows = sorted(per_employee.values(), key=lambda r: r["tin"])
    return {
        "form": "H1",
        "employer_tin": tin_employer,
        "year": year,
        "deadline": date(year + 1, *H1_DEADLINE_MONTH_DAY).isoformat(),
        "rows": rows,
        "totals": {
            "employees": len(rows),
            "gross_kobo": sum(r["gross_kobo"] for r in rows),
            "tax_kobo": sum(r["tax_kobo"] for r in rows),
        },
    }


class PayeReturnStore:
    """One live PAYE schedule per (employer, period); idempotent by key."""

    def __init__(self):
        self._live: dict[tuple[str, str], dict] = {}
        self._by_idem: dict[str, dict] = {}

    def file(self, sched: dict, idempotency_key: str) -> tuple[dict, bool]:
        if idempotency_key in self._by_idem:
            return self._by_idem[idempotency_key], False
        key = (sched["employer_tin"], sched["period"])
        if key in self._live:
            raise PayeError("PAYE schedule already filed for period")
        rec = dict(sched)
        rec.update({"return_id": f"PAYE-{next(_ids):06d}",
                    "filed_at": date.today().isoformat(), "status": "filed"})
        self._live[key] = rec
        self._by_idem[idempotency_key] = rec
        return rec, True

    def for_year(self, employer_tin: str, year: int) -> list[dict]:
        return [s for (t, p), s in sorted(self._live.items())
                if t == employer_tin and p.startswith(f"{year}-")]
