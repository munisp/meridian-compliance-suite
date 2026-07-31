"""I9 — Sector benchmark outlier reports.

Per-sector turnover/tax-ratio distributions; flags taxpayers whose ratio is
>= 2 standard deviations from the sector mean (audit targeting).

REAL: statistics + flagging. Input: taxpayer period summaries.
"""
from __future__ import annotations

from dataclasses import dataclass
from statistics import mean, pstdev


@dataclass
class TaxpayerPeriod:
    tin: str
    sector: str
    turnover_kobo: int
    tax_paid_kobo: int

    @property
    def ratio_bps(self) -> float:
        if self.turnover_kobo <= 0:
            return 0.0
        return self.tax_paid_kobo * 10_000 / self.turnover_kobo


def sector_report(records: list[TaxpayerPeriod], sigma: float = 2.0,
                  min_sector_size: int = 4) -> dict:
    sectors: dict[str, list[TaxpayerPeriod]] = {}
    for r in records:
        sectors.setdefault(r.sector, []).append(r)
    out = {"sectors": {}, "outliers": []}
    for sec, rows in sorted(sectors.items()):
        ratios = [r.ratio_bps for r in rows]
        mu = mean(ratios)
        sd = pstdev(ratios) if len(ratios) > 1 else 0.0
        out["sectors"][sec] = {
            "taxpayers": len(rows), "mean_ratio_bps": round(mu, 2),
            "stddev_bps": round(sd, 2),
            "median_ratio_bps": round(sorted(ratios)[len(ratios) // 2], 2)}
        if len(rows) < min_sector_size or sd == 0:
            continue
        for r, z in zip(rows, ratios):
            zscore = (z - mu) / sd
            if abs(zscore) >= sigma:
                out["outliers"].append({
                    "tin": r.tin, "sector": sec,
                    "ratio_bps": round(z, 2), "sector_mean_bps": round(mu, 2),
                    "z_score": round(zscore, 2),
                    "direction": "below" if zscore < 0 else "above",
                    "reason": "tax-to-turnover ratio |z| >= {} sigma".format(sigma)})
    out["outliers"].sort(key=lambda o: -abs(o["z_score"]))
    return out
