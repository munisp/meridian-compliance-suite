"""I11 — Filing-deadline predictive reminders.

Filing calendar from the rp-fmt-federal pack + per-taxpayer filing history
-> next deadlines -> reminder schedule events (nrs.reminders.due.v1).

REAL: pack-driven deadline resolution + schedule generation with lead days
based on historical lateness. Event emission via the outbox interface
(in-process; Kafka wiring is SIMULATED).
"""
from __future__ import annotations

import calendar
import os
from dataclasses import dataclass
from datetime import date, timedelta
from pathlib import Path

import yaml

PACK_PATH = Path(__file__).resolve().parents[3] / "packages" / "shared" / "rulepack" / "packs" / "rp-fmt-federal" / "1.0.0.yaml"


def _add_months(d: date, n: int) -> date:
    m = d.month - 1 + n
    y = d.year + m // 12
    m = m % 12 + 1
    return date(y, m, min(d.day, calendar.monthrange(y, m)[1]))


def load_calendar(pack_path: Path | None = None) -> dict[str, dict]:
    """tax -> rule 'then' dict, keyed by fmt rule (monthly/annual)."""
    pack = yaml.safe_load((pack_path or PACK_PATH).read_text())
    cal: dict[str, dict] = {}
    for r in pack["rules"]:
        tax = r["when"].get("tax")
        if not tax:
            continue
        # disambiguate rules sharing a tax (beneficiary/filing/document variants)
        extras = ":".join(str(r["when"][k]) for k in sorted(r["when"]) if k != "tax")
        cal[tax + (":" + extras if extras else "")] = r["then"]
    return cal


def next_deadline(rule: dict, period: date, year_end: date | None = None) -> date | None:
    if "deadline_day_of_month" in rule:
        # deadline in the month FOLLOWING the period month
        nxt = _add_months(period.replace(day=1), 1)
        day = min(rule["deadline_day_of_month"], calendar.monthrange(nxt.year, nxt.month)[1])
        return date(nxt.year, nxt.month, day)
    if "deadline_months_after_year_end" in rule and year_end:
        return _add_months(year_end, rule["deadline_months_after_year_end"])
    if "deadline_days" in rule:
        return period + timedelta(days=rule["deadline_days"])
    if "deadline_days_after_year_start" in rule:
        return date(period.year, 1, 1) + timedelta(days=rule["deadline_days_after_year_start"])
    return None


@dataclass
class Reminder:
    tenant_id: str
    tax: str
    period: str
    deadline: str
    remind_on: str
    lead_days: int
    late_history_count: int

    def event(self) -> dict:
        return {
            "type": "nrs.reminders.due.v1",
            "source": "insights.reminders",
            "time": date.today().isoformat() + "T00:00:00Z",
            "tenant_id": self.tenant_id,
            "data": self.__dict__ | {"subject_to_regazette": True},
        }


def schedule(tenant_id: str, tax: str, period: date,
             history: list[dict] | None = None,
             year_end: date | None = None,
             pack_path: Path | None = None) -> Reminder | None:
    """history: past filings [{period, filed_late: bool}] — chronic late filers
    get longer lead time (7d) than compliant ones (3d)."""
    cal = load_calendar(pack_path)
    rule = cal.get(tax)
    if not rule:
        return None
    dl = next_deadline(rule, period, year_end)
    if not dl:
        return None
    late_count = sum(1 for h in (history or []) if h.get("filed_late"))
    lead = 7 if late_count >= 2 else 3
    return Reminder(tenant_id, tax, period.isoformat(), dl.isoformat(),
                    (dl - timedelta(days=lead)).isoformat(), lead, late_count)
