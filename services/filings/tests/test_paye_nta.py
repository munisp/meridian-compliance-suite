"""Regression (R4 S1b#3): PAYE computed only the PITA-legacy regime — 2026+
periods silently applied CRA and the old 7-24% bands. The NTA 2025 regime
(rp-paye-nta@1.0.0, effective 2026-01-01) is now effective-dated: no CRA,
<= N800k exempt, bands 0/15/18/21/23/25, rent relief, no 1% minimum tax.
Legacy pre-2026 periods are unchanged."""
from __future__ import annotations

from datetime import date

from app import paye


def test_nta_minimum_wage_band_exempt():
    # N66,666.67/mo -> N800,000/yr gross: at the exemption threshold.
    r = paye.employee_annual_tax(800_000_00, {}, date(2026, 3, 1))
    assert r["regime"] == "nta-2025"
    assert r["annual_tax_kobo"] == 0
    assert r["cra_kobo"] == 0  # CRA abolished


def test_nta_first_band_zero_then_15pct():
    # N1.2m/yr: first N800k at 0%, remaining N400k at 15% = N60,000/yr.
    r = paye.employee_annual_tax(1_200_000_00, {}, date(2026, 1, 1))
    assert r["taxable_income_kobo"] == 1_200_000_00
    assert r["annual_tax_kobo"] == 60_000_00
    assert r["monthly_tax_kobo"] == 5_000_00


def test_nta_top_earner_full_band_walk():
    # N60m/yr: 0% + 2.2m*15% + 9m*18% + 13m*21% + 25m*23% + 10m*25%
    #        = 330k + 1,620k + 2,730k + 5,750k + 2,500k = N12,930,000.
    r = paye.employee_annual_tax(60_000_000_00, {}, date(2026, 6, 1))
    assert r["annual_tax_kobo"] == 12_930_000_00


def test_nta_rent_relief_capped():
    # 20% of N4m rent = N800k, capped at N500k; deducted before banding.
    r = paye.employee_annual_tax(2_000_000_00,
                                 {"annual_rent_paid_kobo": 4_000_000_00},
                                 date(2026, 1, 1))
    assert r["rent_relief_kobo"] == 500_000_00
    assert r["taxable_income_kobo"] == 1_500_000_00


def test_nta_no_minimum_tax():
    # N840k/yr (> N800k, not exempt): tax only on the N40k slice at 15%;
    # the old 1%-of-gross minimum tax is gone.
    r = paye.employee_annual_tax(840_000_00, {}, date(2026, 2, 1))
    assert r["annual_tax_kobo"] == 6_000_00


def test_legacy_regime_unchanged_pre_2026():
    r = paye.employee_annual_tax(1_200_000_00, {}, date(2025, 1, 1))
    assert r["regime"] == "pitra-legacy"
    assert r["cra_kobo"] > 0  # CRA still applies before 2026
    # legacy: taxable = 1.2m - max(200k, 12k) - 240k = 760k;
    # 300k*7% + 300k*11% + 160k*15% = 21k + 33k + 24k = N78,000
    assert r["annual_tax_kobo"] == 78_000_00


def test_monthly_schedule_uses_nta_for_2026_period():
    sched = paye.build_monthly_schedule(
        "EMP-TIN", "2026-01",
        [{"tin": "t1", "name": "A", "gross_kobo": 100_000_00}])
    # N100k/mo = N1.2m/yr -> N60,000/yr -> N5,000/mo under the NTA.
    assert sched["rows"][0]["tax_kobo"] == 5_000_00
    assert sched["rows"][0]["cra_kobo"] == 0
