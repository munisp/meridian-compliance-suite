"""I14 — Multi-currency FX validation service.

CBN-rate source interface + daily rate table + conversion with audit trail
for cross-border invoices (rp-mbs-business-rules mbs.fx.source-required).

REAL: date-windowed rate lookup, conversion math (integer kobo), audit trail.
SIMULATED: live CBN fetch (CBNRateSource raises without CBN_FX_URL).
"""
from __future__ import annotations

import hashlib
import os
from abc import ABC, abstractmethod
from datetime import date


class RateSource(ABC):
    name = "abstract"

    @abstractmethod
    def rate_bps(self, currency: str, on: date) -> int:
        """Return NGN-per-unit rate in basis points of the quoted unit
        (e.g. 1 USD = 1,550.25 NGN -> 15502500 bps of 100)."""


class StaticRateTable(RateSource):
    """Daily rate table: {currency: [(effective_from, naira_per_unit_x100)]}."""

    name = "static-table"

    def __init__(self, table: dict[str, list[tuple[str, int]]]):
        self.table = {c: sorted((date.fromisoformat(d), r) for d, r in rows)
                      for c, rows in table.items()}

    def rate_bps(self, currency: str, on: date) -> int:
        rows = self.table.get(currency.upper())
        if not rows:
            raise KeyError(f"no rates for {currency}")
        val = None
        for eff, r in rows:
            if on >= eff:
                val = r
        if val is None:
            raise KeyError(f"no {currency} rate on record for {on}")
        return val


class CBNRateSource(RateSource):
    """Live CBN rates (SIMULATED until CBN_FX_URL is configured)."""

    name = "cbn-live"

    def __init__(self, url: str | None = None):
        self.url = url or os.environ.get("CBN_FX_URL", "")
        if not self.url:
            raise RuntimeError("CBNRateSource is SIMULATED: set CBN_FX_URL")

    def rate_bps(self, currency: str, on: date) -> int:  # pragma: no cover
        raise NotImplementedError


# Seed table (dev values, marked subject-to-source-verification)
DEFAULT_TABLE = {
    "USD": [("2026-01-01", 155_000)],
    "EUR": [("2026-01-01", 168_000)],
    "GBP": [("2026-01-01", 196_000)],
}


class FXService:
    def __init__(self, source: RateSource | None = None):
        self.source = source or StaticRateTable(DEFAULT_TABLE)
        self.audit: list[dict] = []

    def convert_to_ngn_kobo(self, amount_minor: int, currency: str,
                            on: date, invoice_id: str = "") -> dict:
        """amount_minor: minor units of foreign currency (e.g. cents).
        Returns NGN kobo with full audit trail."""
        currency = currency.upper()
        if currency == "NGN":
            result = amount_minor
            rate = 10_000
        else:
            rate = self.source.rate_bps(currency, on)
            # minor(2dp) -> naira: amount_minor/100 * rate/10000 naira, x100 kobo
            result = (amount_minor * rate + 50) // 100
        rec = {
            "invoice_id": invoice_id, "currency": currency,
            "amount_minor": amount_minor, "ngn_kobo": result,
            "rate_x100": rate, "rate_date": on.isoformat(),
            "source": self.source.name,
            "digest": hashlib.sha256(
                f"{invoice_id}|{currency}|{amount_minor}|{rate}|{on}".encode()
            ).hexdigest()[:16],
        }
        self.audit.append(rec)
        return rec
