"""I2: TaxProMax CSV export of filed returns for accountants.

nactp reference (`services/tax-professional-service` ExportVATReport,
?format=taxpromax) is a single-row VAT-reconciliation summary with no TIN,
no tax-type column and float money — not a real TaxProMax upload layout.
We therefore define a clean, documented format here instead of copying it:

Columns (one row per filed return / period / tax type):
  TIN, Taxpayer Name, Period, Tax Type, Taxable Amount, Tax Amount,
  Net Payable, Currency, Filing Reference, Status

- money columns are naira with exactly 2 decimal places, converted from
  the integer-kobo filing records with Decimal (half-up) — never float
- Period is YYYY-MM (VAT-002 / PAYE monthly periods)
- Tax Type is VAT | PAYE
- Taxable Amount: VAT -> total sales for the period; PAYE -> gross payroll
- Tax Amount: VAT -> output VAT; PAYE -> total tax deducted
- Net Payable: VAT -> net VAT payable (0 on refund periods; the refund is
  visible via status/amounts, Refund is not a TaxProMax payable column);
  PAYE -> total tax deducted (remittance amount)
- Currency: NGN (TaxProMax uploads are naira-denominated)
- Filing Reference: the service return_id (VAT002-000001 / PAYE-000001)
- Taxpayer Name: carried on the filing record when known; filings do not
  currently capture a legal name, so the column is empty (honest) rather
  than fabricated.

The export is read-only (no idempotency requirement) but every export is
audit-logged (structured log + DocStore `export_audit` collection) with
principal, TIN, period range and row count.
"""
from __future__ import annotations

import csv
import io
import os
import logging
import time
from decimal import ROUND_HALF_UP, Decimal
from typing import Iterable, Iterator

from . import store
from .util import parse_period

log = logging.getLogger("filings.taxpromax")

COLUMNS = [
    "TIN", "Taxpayer Name", "Period", "Tax Type", "Taxable Amount",
    "Tax Amount", "Net Payable", "Currency", "Filing Reference", "Status",
]

TAX_TYPES = ("VAT", "PAYE")


def kobo_to_naira(kobo: int) -> str:
    """Integer kobo -> naira string with exactly 2 decimal places."""
    naira = (Decimal(int(kobo)) / Decimal(100)).quantize(
        Decimal("0.01"), rounding=ROUND_HALF_UP)
    return f"{naira:.2f}"


def _in_range(period: str, from_period: str | None, to_period: str | None) -> bool:
    if from_period and period < from_period:
        return False
    if to_period and period > to_period:
        return False
    return True


# Row cap per export (audit R4 S3#11): an export request previously scanned
# ALL tenants' returns and materialised the full row list in memory — a
# cheap O(DB-size) CPU/memory amplifier. The per-tenant filter is now
# pushed into the store and the result is bounded.
EXPORT_MAX_ROWS = int(os.environ.get("TAXPROMAX_EXPORT_MAX_ROWS", "10000"))


def collect_rows(vat_store, paye_store, tin: str,
                 from_period: str | None = None, to_period: str | None = None,
                 tax_type: str | None = None,
                 max_rows: int | None = None) -> tuple[list[list[str]], bool]:
    """Materialise export rows for one TIN, newest stores win per period.
    Returns (rows, truncated): the per-tenant query is pushed into the store
    and bounded at max_rows+1 so truncation is detected without an unbounded
    read."""
    if max_rows is None:
        max_rows = EXPORT_MAX_ROWS
    if max_rows < 1:
        max_rows = 1
    if from_period:
        parse_period(from_period)
    if to_period:
        parse_period(to_period)
    wanted = (tax_type.upper(),) if tax_type else TAX_TYPES
    for t in wanted:
        if t not in TAX_TYPES:
            raise ValueError(f"unknown tax_type {tax_type!r}; expected VAT|PAYE")

    truncated = False
    rows: list[list[str]] = []
    if "VAT" in wanted:
        recs = vat_store._docs.query("vat_returns", {"tin": tin}, max_rows + 1)
        if len(recs) > max_rows:
            truncated = True
            recs = recs[:max_rows]
        for rec in recs:
            if not _in_range(rec["period"], from_period, to_period):
                continue
            rows.append([
                tin,
                rec.get("taxpayer_name", ""),
                rec["period"],
                "VAT",
                kobo_to_naira(rec["sales_schedule"]["total_sales_kobo"]),
                kobo_to_naira(rec["output_vat_kobo"]),
                kobo_to_naira(rec["net_vat_payable_kobo"]),
                "NGN",
                rec["return_id"],
                rec["status"],
            ])
    if "PAYE" in wanted:
        recs = paye_store._docs.query("paye_returns", {"employer_tin": tin}, max_rows + 1)
        if len(recs) > max_rows:
            truncated = True
            recs = recs[:max_rows]
        for rec in recs:
            if not _in_range(rec["period"], from_period, to_period):
                continue
            rows.append([
                tin,
                rec.get("taxpayer_name", ""),
                rec["period"],
                "PAYE",
                kobo_to_naira(rec["totals"]["gross_kobo"]),
                kobo_to_naira(rec["totals"]["tax_kobo"]),
                kobo_to_naira(rec["totals"]["tax_kobo"]),
                "NGN",
                rec["return_id"],
                rec["status"],
            ])
    rows.sort(key=lambda r: (r[2], r[3]))  # period, tax type
    return rows, truncated


def stream_csv(rows: Iterable[list[str]]) -> Iterator[str]:
    """Stream the CSV: header line always, then one chunk per row."""
    buf = io.StringIO()
    writer = csv.writer(buf, lineterminator="\r\n")
    writer.writerow(COLUMNS)
    yield buf.getvalue()
    for row in rows:
        buf.seek(0)
        buf.truncate(0)
        writer.writerow(row)
        yield buf.getvalue()


def audit_export(docs: store.DocStore, principal_sub: str, tin: str,
                 from_period: str | None, to_period: str | None,
                 tax_type: str | None, row_count: int) -> None:
    """Audit-log every export (platform audit convention: immutable record +
    structured log)."""
    entry = {
        "event": "taxpromax_export",
        "principal": principal_sub,
        "tin": tin,
        "from_period": from_period,
        "to_period": to_period,
        "tax_type": tax_type,
        "row_count": row_count,
        "at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }
    rid = f"{entry['at']}|{principal_sub}|{tin}|{row_count}"
    try:
        docs.put("export_audit", rid, entry)
    except Exception as e:  # audit must never break the export
        log.warning("export audit persist failed: %s", e)
    log.info("audit event=%s principal=%s tin=%s range=%s..%s tax_type=%s rows=%d",
             entry["event"], principal_sub, tin, from_period or "",
             to_period or "", tax_type or "", row_count)
