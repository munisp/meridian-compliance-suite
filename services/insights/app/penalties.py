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
# Late payment: % of tax due (bps) per tax type, effective-dated.
# AUDIT FIX (parity4/gov-filing-gaps §2): the previous WHT rate of 40%
# citing "NTAA s.74" was wrong — gazetted NTAA s.74 is not a WHT offence
# provision (advance rulings / digital assets pagination). The gazette-
# aligned general late-payment rule is NTAA s.65: 10% of the tax due PLUS
# interest at CBN MPR (PwC Tax Summaries; Banwo & Ighodalo s.65 summary).
# 40% appears to be a legacy CITA-era figure. Corrected to 1000 bps,
# effective from NTAA commencement (2026-01-01).
# (effective_from, bps) per tax type — resolution picks latest row <= due.
LATE_PAYMENT_BPS = {
    "VAT": [(date(2026, 1, 1), 1000)],
    "CIT": [(date(2026, 1, 1), 1000)],
    "PIT": [(date(2026, 1, 1), 1000)],
    "WHT": [(date(2026, 1, 1), 1000)],  # was 4000 — see audit note above
}

# Failure to register (NTAA 2025 s.100(1), gazette text): N50,000 in the
# first month of default + N25,000 for EACH subsequent month the failure
# continues (AfricaCheck factsheet; Banwo & Ighodalo).
REGISTER_FAILURE = (date(2026, 1, 1), 5_000_000, 2_500_000)  # kobo

# Contracting an unregistered person (NTAA s.100(2)): N5,000,000 penalty on
# the agency/company per engagement.
UNREGISTERED_CONTRACT_KOBO = 500_000_000_00  # N5,000,000


def registration_penalty(months_unregistered: int, when: date) -> int:
    """NTAA s.100(1) tiered failure-to-register penalty (integer kobo).
    Fail-closed: no penalty rows before the NTAA effective date."""
    if months_unregistered <= 0:
        return 0
    eff, first, per_month = REGISTER_FAILURE
    if when < eff:
        raise ValueError("no registration-penalty rule on record for %s" % when)
    return first + per_month * (months_unregistered - 1)


def unregistered_contract_penalty(engagements: int, when: date) -> int:
    """NTAA s.100(2): N5m per engagement of an unregistered person."""
    if engagements <= 0:
        return 0
    eff, _, _ = REGISTER_FAILURE
    if when < eff:
        raise ValueError("no registration-penalty rule on record for %s" % when)
    return UNREGISTERED_CONTRACT_KOBO * engagements

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
        payment_bps = _rate_at(LATE_PAYMENT_BPS[tax_type], due)[0]
        payment_kobo = tax_kobo * payment_bps // 10_000
        # simple interest, ACT/365, rate fixed at MPR in force on due date
        interest_kobo = tax_kobo * (mpr + SPREAD_BPS) * days_late // (10_000 * 365)
    return PenaltyResult(tax_type, months, days_late, filing_kobo,
                         payment_kobo, interest_kobo,
                         filing_kobo + payment_kobo + interest_kobo, mpr)
