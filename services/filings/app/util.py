"""Shared helpers for the filings service.

All monetary values are integer kobo. Any rate multiplication goes through
``kobo_mul`` which rounds HALF-UP to the nearest kobo (never banker's
rounding, never floats).

REAL: deadline calendar, kobo rounding, period handling.
"""
from __future__ import annotations

from datetime import date
from decimal import ROUND_HALF_UP, Decimal


def kobo_mul(amount_kobo: int, bps: int) -> int:
    """amount * bps / 10_000, rounded half-up to integer kobo."""
    amt = Decimal(int(amount_kobo)) * Decimal(int(bps))
    return int((amt / Decimal(10_000)).quantize(Decimal("1"), rounding=ROUND_HALF_UP))


def round_half_up(value: Decimal) -> int:
    return int(value.quantize(Decimal("1"), rounding=ROUND_HALF_UP))


def parse_period(period: str) -> tuple[int, int]:
    """'YYYY-MM' -> (year, month); raises ValueError on malformed input."""
    parts = period.split("-")
    if len(parts) != 2:
        raise ValueError(f"period must be YYYY-MM, got {period!r}")
    year, month = int(parts[0]), int(parts[1])
    if not 1 <= month <= 12:
        raise ValueError(f"month out of range in period {period!r}")
    return year, month


def period_end(period: str) -> date:
    year, month = parse_period(period)
    if month == 12:
        return date(year, 12, 31)
    return date(year, month + 1, 1).replace(day=1) - _ONE_DAY


_ONE_DAY = __import__("datetime").timedelta(days=1)


def deadline_nth_of_following_month(period: str, day: int) -> date:
    """Filing deadline = `day`-th of the month following the period."""
    year, month = parse_period(period)
    if month == 12:
        return date(year + 1, 1, day)
    return date(year, month + 1, day)
