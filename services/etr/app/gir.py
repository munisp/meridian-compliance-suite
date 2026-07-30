"""GIR XML builder + filing-pack JSON builder (T9).

GIR structure follows rp-gir-schema required sections (OECD GloBE Information
Return). Filing-pack JSON bundles computation + step trace + entity roster +
pack versions + digest (audit-defensible).
"""
from __future__ import annotations

import hashlib
import json
import xml.etree.ElementTree as ET
from xml.dom import minidom

from .models import Computation, ConstituentEntity, Group
from .packs import PackSet


def build_gir_xml(ps: PackSet, comp: Computation, group: Group,
                  entities: list[ConstituentEntity]) -> str:
    root = ET.Element("GIR", {
        "xmlns": "urn:oecd:ties:globe:gir:v1",
        "version": "1.0",
        "messageRefId": comp.id,
        "timestamp": comp.created_at,
    })
    spec = ET.SubElement(root, "MessageSpec")
    ET.SubElement(spec, "MessageType").text = "GIR"
    ET.SubElement(spec, "TransmittingCountry").text = "NG"
    ET.SubElement(spec, "ReportingPeriod").text = f"{comp.fiscal_year}-12-31"

    mne = ET.SubElement(root, "MNEGroup")
    ET.SubElement(mne, "GroupRef").text = group.id
    ET.SubElement(mne, "Name").text = group.name
    ET.SubElement(mne, "UPEJurisdiction").text = group.upe_jurisdiction
    ET.SubElement(mne, "ConsolidatedRevenueKobo").text = str(group.consolidated_revenue_kobo)
    ET.SubElement(mne, "InScope").text = "true" if comp.in_scope else "false"
    ET.SubElement(mne, "FiscalYear").text = str(comp.fiscal_year)
    ET.SubElement(mne, "Basis").text = comp.basis

    ces = ET.SubElement(root, "ConstituentEntities")
    for e in entities:
        ce = ET.SubElement(ces, "ConstituentEntity", {"id": e.id})
        ET.SubElement(ce, "Name").text = e.name
        ET.SubElement(ce, "Jurisdiction").text = e.jurisdiction
        if e.parent_id:
            ET.SubElement(ce, "ParentRef").text = e.parent_id
        ET.SubElement(ce, "OwnershipBps").text = str(e.ownership_bps)
        ET.SubElement(ce, "IsUPE").text = "true" if e.is_upe else "false"

    for jr in comp.jurisdictions:
        j = ET.SubElement(root, "JurisdictionETR", {"jurisdiction": jr.jurisdiction})
        ET.SubElement(j, "NetIncomeKobo").text = str(jr.net_income_kobo)
        ET.SubElement(j, "CoveredTaxesKobo").text = str(jr.covered_taxes_kobo)
        ET.SubElement(j, "ETRBps").text = str(jr.etr_bps)
        ET.SubElement(j, "TopupPctBps").text = str(jr.topup_pct_bps)
        ET.SubElement(j, "TopupKobo").text = str(jr.topup_kobo)
        sbie = ET.SubElement(j, "SBIEDetail")
        ET.SubElement(sbie, "SBIEKobo").text = str(jr.sbie_kobo)
        ET.SubElement(sbie, "ExcessProfitKobo").text = str(jr.excess_profit_kobo)
        if jr.qdmtt_applied:
            q = ET.SubElement(j, "QDMTT")
            ET.SubElement(q, "AmountKobo").text = str(jr.qdmtt_kobo)

    alloc_el = ET.SubElement(root, "TopupAllocation")
    for a in comp.iir_allocations:
        al = ET.SubElement(alloc_el, "Allocation", {"mechanism": a.mechanism})
        ET.SubElement(al, "FromJurisdiction").text = a.from_jurisdiction
        ET.SubElement(al, "ToEntityRef").text = a.to_entity_id
        ET.SubElement(al, "ToEntityName").text = a.to_entity_name
        ET.SubElement(al, "AmountKobo").text = str(a.amount_kobo)
        ET.SubElement(al, "OwnershipBps").text = str(a.ownership_bps)

    meta = ET.SubElement(root, "PackVersions")
    for pid, ver in comp.pack_versions.items():
        ET.SubElement(meta, "Pack", {"id": pid}).text = ver
    ET.SubElement(root, "Digest").text = comp.digest

    rough = ET.tostring(root, encoding="utf-8")
    return minidom.parseString(rough).toprettyxml(indent="  ")


def build_filing_pack(ps: PackSet, comp: Computation, group: Group,
                      entities: list[ConstituentEntity]) -> dict:
    pack = {
        "filing_pack_id": f"fp-{comp.id}",
        "generated_at": comp.created_at,
        "group": group.model_dump(),
        "computation": comp.model_dump(exclude={"trace"}),
        "step_trace": [s.model_dump() for s in comp.trace],
        "entity_roster": [e.model_dump() for e in entities],
        "pack_versions": comp.pack_versions,
        "gir_sections": ps.gir_sections(),
        "digest": comp.digest,
    }
    pack["filing_pack_digest"] = hashlib.sha256(
        json.dumps(pack, sort_keys=True).encode()).hexdigest()
    return pack
