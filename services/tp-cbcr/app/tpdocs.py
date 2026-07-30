"""Master file / local file document assembly (SPEC 3 T8): structured JSON
document model plus an HTML renderer. Content structure follows OECD
BEPS Action 13 / Nigeria TP Regulations 2018 chapter requirements."""

from __future__ import annotations

import html

MASTER_FILE_SECTIONS = [
    ("group_structure", "Organisational structure of the MNE group"),
    ("business_description", "Description of the MNE's business(es)"),
    ("intangibles", "MNE's intangibles"),
    ("financing", "MNE's intercompany financial activities"),
    ("financial_tax", "MNE's financial and tax positions"),
]

LOCAL_FILE_SECTIONS = [
    ("entity_overview", "Local entity overview"),
    ("controlled_transactions", "Controlled transactions detail"),
    ("financial_information", "Local entity financial information"),
    ("comparability", "Comparability analysis"),
    ("tp_method", "Transfer pricing method selection and application"),
]


def assemble_master_file(group: dict, entities: list[dict],
                         transactions: list[dict]) -> dict:
    """Structured master file document."""
    jurisdictions = sorted({e.get("jurisdiction", "NG") for e in entities})
    return {
        "document": "master_file",
        "standard": "OECD BEPS Action 13 / Nigeria TP Regulations 2018 (Reg 8)",
        "group": {
            "name": group.get("name", ""),
            "ultimate_parent_tin": group.get("ultimate_parent_tin", ""),
            "reporting_period": group.get("reporting_period", ""),
        },
        "sections": {
            "group_structure": {
                "title": dict(MASTER_FILE_SECTIONS)["group_structure"],
                "entities": entities,
                "jurisdictions": jurisdictions,
                "entity_count": len(entities),
            },
            "business_description": {
                "title": dict(MASTER_FILE_SECTIONS)["business_description"],
                "drivers": group.get("business_drivers", []),
                "supply_chain": group.get("supply_chain", ""),
                "key_markets": jurisdictions,
            },
            "intangibles": {
                "title": dict(MASTER_FILE_SECTIONS)["intangibles"],
                "strategy": group.get("intangibles_strategy", ""),
                "registered_ip": group.get("registered_ip", []),
            },
            "financing": {
                "title": dict(MASTER_FILE_SECTIONS)["financing"],
                "intercompany_loans": [
                    t for t in transactions
                    if t.get("tx_type") in ("loan", "interest")],
                "financing_policy": group.get("financing_policy", ""),
            },
            "financial_tax": {
                "title": dict(MASTER_FILE_SECTIONS)["financial_tax"],
                "consolidated_revenue_kobo":
                    group.get("consolidated_revenue_kobo", 0),
                "cbcr_required": group.get("cbcr_required", False),
                "rulings": group.get("rulings", []),
            },
        },
    }


def assemble_local_file(entity: dict, transactions: list[dict],
                        financials: dict) -> dict:
    """Structured local file document for one constituent entity."""
    related = [t for t in transactions
               if t.get("from_tin") == entity.get("tin")
               or t.get("to_tin") == entity.get("tin")]
    by_type: dict[str, int] = {}
    for t in related:
        by_type[t.get("tx_type", "other")] = \
            by_type.get(t.get("tx_type", "other"), 0) + int(t.get("amount_kobo", 0))
    return {
        "document": "local_file",
        "standard": "OECD BEPS Action 13 / Nigeria TP Regulations 2018 (Reg 8)",
        "entity": entity,
        "sections": {
            "entity_overview": {
                "title": dict(LOCAL_FILE_SECTIONS)["entity_overview"],
                "management_structure": financials.get("management", ""),
                "business_strategy": financials.get("strategy", ""),
            },
            "controlled_transactions": {
                "title": dict(LOCAL_FILE_SECTIONS)["controlled_transactions"],
                "transactions": related,
                "totals_by_type_kobo": by_type,
                "total_kobo": sum(by_type.values()),
            },
            "financial_information": {
                "title": dict(LOCAL_FILE_SECTIONS)["financial_information"],
                "revenue_kobo": financials.get("revenue_kobo", 0),
                "ebitda_kobo": financials.get("ebitda_kobo", 0),
                "profit_kobo": financials.get("profit_kobo", 0),
            },
            "comparability": {
                "title": dict(LOCAL_FILE_SECTIONS)["comparability"],
                "comparables": financials.get("comparables", []),
                "method_benchmark": financials.get("benchmark", ""),
            },
            "tp_method": {
                "title": dict(LOCAL_FILE_SECTIONS)["tp_method"],
                "method": financials.get("tp_method", "CUP"),
                "rationale": financials.get("method_rationale", ""),
            },
        },
    }


def render_html(doc: dict) -> str:
    """Render a structured document as a clean HTML page."""
    def esc(x):
        return html.escape(str(x))

    parts = [
        "<!DOCTYPE html><html><head><meta charset='utf-8'>",
        f"<title>{esc(doc['document'].replace('_', ' ').title())}</title>",
        "<style>body{font-family:system-ui,sans-serif;max-width:860px;margin:"
        "2rem auto;color:#2b2620;background:#faf8f5}h1{border-bottom:2px "
        "solid #8a6d3b;padding-bottom:.3rem}h2{color:#6b5537;margin-top:"
        "2rem}table{border-collapse:collapse;width:100%;margin:.5rem 0}"
        "td,th{border:1px solid #d8cfc2;padding:.35rem .6rem;text-align:left}"
        "th{background:#efe9e0}.meta{color:#7d7466;font-size:.9rem}</style>",
        "</head><body>",
        f"<h1>{esc(doc['document'].replace('_', ' ').title())}</h1>",
        f"<p class='meta'>{esc(doc['standard'])}</p>",
    ]
    subject = doc.get("group") or doc.get("entity") or {}
    parts.append("<table>")
    for k, v in subject.items():
        parts.append(f"<tr><th>{esc(k)}</th><td>{esc(v)}</td></tr>")
    parts.append("</table>")
    for key, section in doc.get("sections", {}).items():
        parts.append(f"<h2>{esc(section['title'])}</h2>")
        for sk, sv in section.items():
            if sk == "title":
                continue
            if isinstance(sv, list):
                parts.append(f"<h3>{esc(sk)}</h3><ul>")
                for item in sv:
                    parts.append(f"<li><code>{esc(item)}</code></li>")
                parts.append("</ul>")
            elif isinstance(sv, dict):
                parts.append(f"<h3>{esc(sk)}</h3><table>")
                for dk, dv in sv.items():
                    parts.append(f"<tr><th>{esc(dk)}</th><td>{esc(dv)}</td></tr>")
                parts.append("</table>")
            else:
                parts.append(f"<p><strong>{esc(sk)}:</strong> {esc(sv)}</p>")
    parts.append("</body></html>")
    return "\n".join(parts)
