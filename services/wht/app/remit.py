"""WHT remittance file generation (SPEC 3 T7): CSV + XML schedules of
deductions for remittance to NRS (companies) / State IRS (individuals)."""

from __future__ import annotations

import csv
import io
import xml.etree.ElementTree as ET
from xml.dom import minidom

FIELDS = ["vendor_tin", "vendor_name", "payment_type", "beneficiary",
          "amount_kobo", "rate_bps", "wht_kobo", "deduction_date",
          "period", "outcome"]


def _rows(deductions: list[dict]) -> list[dict]:
    return [{k: d.get(k, "") for k in FIELDS} for d in deductions]


def remittance_csv(batch_id: str, deductions: list[dict]) -> str:
    buf = io.StringIO()
    writer = csv.DictWriter(buf, fieldnames=["batch_id"] + FIELDS)
    writer.writeheader()
    for row in _rows(deductions):
        writer.writerow({"batch_id": batch_id, **row})
    total = sum(int(d["wht_kobo"]) for d in deductions)
    writer.writerow({"batch_id": batch_id, "vendor_tin": "TOTAL",
                     "wht_kobo": total})
    return buf.getvalue()


def remittance_xml(batch_id: str, deductions: list[dict], period: str) -> str:
    root = ET.Element("WhtRemittance", {
        "batchId": batch_id, "period": period, "currency": "NGN",
        "schema": "NRS-WHT-REM-1.0"})
    for d in deductions:
        entry = ET.SubElement(root, "Deduction")
        for k in FIELDS:
            child = ET.SubElement(entry, k)
            child.text = str(d.get(k, ""))
    total = sum(int(d["wht_kobo"]) for d in deductions)
    ET.SubElement(root, "TotalWhtKobo").text = str(total)
    ET.SubElement(root, "DeductionCount").text = str(len(deductions))
    raw = ET.tostring(root, encoding="unicode")
    return minidom.parseString(raw).toprettyxml(indent="  ")
