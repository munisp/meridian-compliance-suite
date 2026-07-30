"""OECD CbCR XML generator (SPEC 3 T8): real namespaced XML following the
OECD CbC XML Schema v2.0 structure (urn:oecd:ties:cbc:v2 with the STF
namespace for DocSpec)."""

from __future__ import annotations

import time
import uuid
import xml.etree.ElementTree as ET
from xml.dom import minidom

NS_CBC = "urn:oecd:ties:cbc:v2"
NS_STF = "urn:oecd:ties:stf:v4"

ET.register_namespace("", NS_CBC)
ET.register_namespace("stf", NS_STF)


def _el(parent, tag, text=None, **attrs):
    e = ET.SubElement(parent, f"{{{NS_CBC}}}{tag}", attrs)
    if text is not None:
        e.text = str(text)
    return e


def _stf(parent, tag, text=None):
    e = ET.SubElement(parent, f"{{{NS_STF}}}{tag}")
    if text is not None:
        e.text = str(text)
    return e


def _doc_spec(parent, ref_id: str, indic: str = "OECD1"):
    ds = _el(parent, "DocSpec")
    _stf(ds, "DocTypeIndic", indic)
    _stf(ds, "DocRefId", ref_id)
    return ds


def _amount(parent, tag, value_minor: int, curr: str = "NGN"):
    # CbCR amounts are in whole units of the reporting currency
    whole = value_minor // 100
    return _el(parent, tag, whole, currCode=curr)


def build_cbcr_xml(report: dict) -> str:
    """Build a CbCR XML document from a report payload:

    report = {
      "message": {"sending_entity_in": ..., "warning": ...},
      "reporting_period": "2025-12-31",
      "reporting_entity": {"tin","name","jurisdiction","biz_activity"},
      "jurisdictions": [
        {"country": "NG",
         "revenue_unrelated_kobo": int, "revenue_related_kobo": int,
         "profit_or_loss_kobo": int, "tax_paid_kobo": int,
         "tax_accrued_kobo": int, "capital_kobo": int,
         "earnings_kobo": int, "employees": int, "assets_kobo": int,
         "constituent_entities": [{"tin","name","biz_activity"}]}
      ],
      "currency": "NGN"
    }
    """
    curr = report.get("currency", "NGN")
    root = ET.Element(f"{{{NS_CBC}}}CbC_OECD", {"version": "2.0"})

    msg = _el(root, "MessageSpec")
    msg_in = report.get("message", {})
    _el(msg, "SendingEntityIN", msg_in.get("sending_entity_in",
        report["reporting_entity"]["tin"]))
    _el(msg, "TransmittingCountry", "NG")
    _el(msg, "ReceivingCountry", "NG")
    _el(msg, "MessageType", "CBC")
    _el(msg, "Language", "EN")
    if msg_in.get("warning"):
        _el(msg, "Warning", msg_in["warning"])
    _el(msg, "MessageRefId", f"NG-{uuid.uuid4().hex[:20]}")
    _el(msg, "MessageTypeIndic", "CBC401")
    _el(msg, "ReportingPeriod", report["reporting_period"])
    _el(msg, "Timestamp", time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))

    body = _el(root, "CbCBody")
    ent = _el(body, "ReportingEntity")
    re_ = report["reporting_entity"]
    _doc_spec(ent, f"RE-{re_['tin']}-{report['reporting_period']}")
    _el(ent, "ResCountryCode", re_.get("jurisdiction", "NG"))
    _el(ent, "TIN", re_["tin"])
    _el(ent, "Name", re_["name"])
    addr = _el(ent, "Address")
    _el(addr, "CountryCode", re_.get("jurisdiction", "NG"))
    _el(addr, "AddressFree", re_.get("address", ""))
    for act in re_.get("biz_activities", [re_.get("biz_activity", "CBC501")]):
        _el(ent, "BizActivities", act)

    reports_el = _el(body, "CbCReports")
    for jur in report.get("jurisdictions", []):
        rep = _el(reports_el, "CbCReport")
        _doc_spec(rep, f"RPT-{jur['country']}-{report['reporting_period']}")
        _el(rep, "ResCountryCode", jur["country"])
        summ = _el(rep, "Summary")
        rev = _el(summ, "Revenue")
        _amount(rev, "Unrelated", int(jur.get("revenue_unrelated_kobo", 0)), curr)
        _amount(rev, "Related", int(jur.get("revenue_related_kobo", 0)), curr)
        total = int(jur.get("revenue_unrelated_kobo", 0)) + \
            int(jur.get("revenue_related_kobo", 0))
        _amount(rev, "Total", total, curr)
        _amount(summ, "ProfitOrLoss", int(jur.get("profit_or_loss_kobo", 0)), curr)
        _amount(summ, "TaxPaid", int(jur.get("tax_paid_kobo", 0)), curr)
        _amount(summ, "TaxAccrued", int(jur.get("tax_accrued_kobo", 0)), curr)
        _amount(summ, "Capital", int(jur.get("capital_kobo", 0)), curr)
        _amount(summ, "Earnings", int(jur.get("earnings_kobo", 0)), curr)
        _el(summ, "Employees", int(jur.get("employees", 0)))
        _amount(summ, "Assets", int(jur.get("assets_kobo", 0)), curr)
        consts = _el(rep, "ConstEntities")
        for ce in jur.get("constituent_entities", []):
            ce_el = _el(consts, "ConstEntity")
            _el(ce_el, "ResCountryCode", ce.get("jurisdiction", jur["country"]))
            _el(ce_el, "TIN", ce["tin"])
            _el(ce_el, "Name", ce["name"])
            addr = _el(ce_el, "Address")
            _el(addr, "CountryCode", ce.get("jurisdiction", jur["country"]))
            for act in ce.get("biz_activities",
                              [ce.get("biz_activity", "CBC501")]):
                _el(ce_el, "BizActivities", act)

    raw = ET.tostring(root, encoding="unicode")
    pretty = minidom.parseString(raw).toprettyxml(indent="  ")
    return pretty
