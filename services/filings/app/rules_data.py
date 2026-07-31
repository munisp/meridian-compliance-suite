"""Effective-dated rate/rule tables for periodic filings.

These tables are the canonical pack rules (meridian-rule-packs) inlined as
data because the periodic filing layer is not yet pack-encoded there.

*** RULE-PACK SYNC REQUIREMENT ***
When meridian-rule-packs adds rp-* packs covering these computations
(capital allowance, CIT tiers, loss relief, minimum tax, dev levy, VAT
return netting), this module MUST be switched to load the packs and these
tables retired or drift-guarded like services/pos-vat/embedded_packs.go.

All rows are (effective_from, payload); resolution picks the latest row
whose effective_from <= the reference date (fail-closed: unknown era
raises). Amounts in kobo; rates in basis points unless noted.

Sources: NTA 2025; NTAA 2025 (gazette No. 117 Vol. 112, 26 Jun 2025);
rp-ntaa-penalties / rp-fmt-federal / rp-education-ng / rp-paye-pitra-legacy.
"""
from __future__ import annotations

from datetime import date

# --- VAT (rp-vat-rates: 7.5% standard; zero-rated recovers input, exempt does not)
VAT_RATE_BPS = [
    (date(2020, 2, 1), 750),   # Finance Act 2019; retained by NTA 2025 s.146
]

# --- CIT tiers (NTA 2025): small company 0%, standard 30%.
# small = annual turnover <= threshold AND total fixed assets <= threshold.
CIT_SMALL_TURNOVER_THRESHOLD_KOBO = [
    (date(2026, 1, 1), 100_000_000_00),   # N100m (NTA 2025)
    (date(2020, 1, 1), 25_000_000_00),    # N25m (Finance Act 2019 legacy)
]
CIT_SMALL_ASSET_THRESHOLD_KOBO = [
    (date(2026, 1, 1), 250_000_000_00),   # N250m (NTA 2025)
]
CIT_RATE_BPS = [
    (date(2026, 1, 1), {"small": 0, "standard": 3000}),
    (date(2007, 1, 1), {"small": 2000, "standard": 3000}),  # CITA legacy tiers
]

# --- Capital allowance rates by asset class (CITA 2nd Schedule, carried
# into NTA 2025 schedules). ia = initial allowance, aa = annual allowance.
CAPITAL_ALLOWANCE_RATES_BPS = [
    (date(2007, 1, 1), {
        "industrial_building":  {"ia": 1500, "aa": 1000},
        "non_industrial_building": {"ia": 1500, "aa": 1000},
        "plant_machinery":      {"ia": 5000, "aa": 2500},
        "motor_vehicle":        {"ia": 5000, "aa": 2500},
        "furniture_fittings":   {"ia": 2500, "aa": 2000},
        "other":                {"ia": 2500, "aa": 2000},
    }),
]

# --- Loss relief: pre-NTA rule capped carryforward at 4 years for losses
# (CITA s.31 legacy, applicable to non-agricultural/insurance trades);
# NTA 2025 removes the time cap (indefinite carryforward, effective
# 2026-01-01). Keep BOTH rows; resolution is by the year the loss AROSE.
LOSS_RELIEF_MAX_CARRYFORWARD_YEARS = [
    (date(2026, 1, 1), None),   # NTA 2025: no time cap
    (date(1990, 1, 1), 4),      # pre-NTA: 4-year carryforward limit
]

# --- Minimum tax floor (CITA s.33 / NTA 2025): % of gross turnover; small
# companies exempt.
MINIMUM_TAX_TURNOVER_BPS = [
    (date(2026, 1, 1), 50),     # 0.5% of turnover
]

# --- Development levy: 4% of assessable profit, co-filed with CIT
# (rp-education-ng devlevy.assessable-profit; NTA 2025, effective 2026).
DEV_LEVY_ASSESSABLE_PROFIT_BPS = [
    (date(2026, 1, 1), 400),
]

# --- PAYE (rp-paye-pitra-legacy): CRA, exempt deductions, 7-24% bands,
# 1% minimum tax. Bands are ANNUAL, in kobo.
PAYE_CRA = [
    # (effective_from, fixed_kobo, pct_of_gross_bps)
    (date(2011, 1, 1), (200_000_00, 2000)),  # higher of N200k or 1% gross, + 20% gross
]
PAYE_BANDS = [
    (date(2011, 1, 1), [
        (300_000_00, 700),
        (300_000_00, 1100),
        (500_000_00, 1500),
        (500_000_00, 1900),
        (1_600_000_00, 2100),
        (None, 2400),           # above N3.2m
    ]),
]
PAYE_MINIMUM_TAX_BPS = [
    (date(2011, 1, 1), 100),    # 1% of gross where taxable income is nil/negligible
]
# Statutory exempt deductions (pension, NHF, NHIS, life assurance, gratuity)
PAYE_EXEMPT_DEDUCTION_KEYS = ("pension_kobo", "nhf_kobo", "nhis_kobo",
                              "life_assurance_kobo", "gratuity_kobo")


def resolve(table, when: date):
    """Latest row with effective_from <= when; fail-closed if none."""
    row = None
    for eff, payload in table:
        if when >= eff and (row is None or eff > row[0]):
            row = (eff, payload)
    if row is None:
        raise ValueError(f"no rule row on record for {when} (fail-closed)")
    return row[1]
