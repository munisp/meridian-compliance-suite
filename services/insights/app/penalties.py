"""I10 — Penalty/interest auto-computation engine.

Gazette-rate tables (NTAA 2025) for late filing and late payment per tax
type, plus interest accrual at CBN MPR + spread, effective-date aware.

REAL: date-windowed rate resolution, month accrual, day-count interest.
Rates: rp-ntaa-penalties (NTAA 2025). MPR table seeded with published CBN
decisions; extend via MPR_TABLE_JSON env.
"""
from __future__ import annotations

import json
import os
from dataclasses import dataclass
from datetime import date

# (effective_from, penalty_kobo_first_month, penalty_kobo_per_month_after)
LATE_FILING = {
    "VAT":  (date(2026, 1, 1), 10_000_000, 5_000_000),   # N100k + N50k/mo
    "CIT":  (date(2026, 1, 1), 10_000_000, 5_000_000),
    "WHT":  (date(2026, 1, 1), 10_000_000, 5_000_000),
    "PIT":  (date(2026, 1, 1), 10_000_000, 5_000_000),
}
# Late payment: % of tax due (bps) per tax type (WHT: 40% one-off, NTAA s.74)
LATE_PAYMENT_BPS = {"VAT": 1000, "CIT": 1000, "PIT": 1000, "WHT": 4000}

# (effective_from, mpr_bps) — CBN MPC decisions (published).
MPR_TABLE = [
    (date(2024, 11, 26), 2750),
    (date(2025, 9, 23), 2700),
]
SPREAD_BPS = 0  # NTAA: interest at MPR (spread kept as parameter)


@dataclass
class PenaltyResult:
    tax_type: str
    months_late_filing: int
    days_late_payment: int
    late_filing_kobo: int
    late_payment_kobo: int
    interest_kobo: int
    total_kobo: int
    mpr_bps_used: int

    def as_dict(self) -> dict:
        return self.__dict__


def _rate_at(table, when: date):
    row = None
    for eff, *vals in sorted(table if isinstance(table, list) else []):
        if when >= eff:
            row = vals
    return row


def mpr_at(when: date) -> int:
    override = os.environ.get("MPR_TABLE_JSON")
    table = MPR_TABLE
    if override:
        table = [(date.fromisoformat(d), b) for d, b in json.loads(override)]
    row = _rate_at(table, when)
    if row is None:
        raise ValueError("no MPR on record for %s" % when)
    return row[0]


def months_between(due: date, done: date) -> int:
    """Whole months (or part) of lateness, NTAA style: any started month counts."""
    if done <= due:
        return 0
    months = (done.year - due.year) * 12 + (done.month - due.month)
    if done.day > due.day:
        months += 1
    return max(months, 1)


def compute(tax_type: str, due: date, filed: date | None, paid: date | None,
            tax_kobo: int, today: date | None = None) -> PenaltyResult:
    tax_type = tax_type.upper()
    today = today or date.today()
    filed = filed or today
    paid = paid or today
    eff, first, per_month = LATE_FILING[tax_type]
    filing_kobo = 0
    months = months_between(due, filed)
    if due >= eff and months:
        filing_kobo = first + per_month * (months - 1)
    days_late = max((paid - due).days, 0)
    payment_kobo = 0
    interest_kobo = 0
    mpr = mpr_at(due)
    if days_late and tax_kobo > 0:
        payment_kobo = tax_kobo * LATE_PAYMENT_BPS[tax_type] // 10_000
        # simple interest, ACT/365, rate fixed at MPR in force on due date
        interest_kobo = tax_kobo * (mpr + SPREAD_BPS) * days_late // (10_000 * 365)
    return PenaltyResult(tax_type, months, days_late, filing_kobo,
                         payment_kobo, interest_kobo,
                         filing_kobo + payment_kobo + interest_kobo, mpr)
