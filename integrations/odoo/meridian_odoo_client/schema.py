"""NRS UBL-shaped JSON payload construction from a neutral (Odoo-shaped)
invoice mapping.

Mirrors services/einvoicing/nrs_schema.go and nrs_irn.go:

  * All money math happens in INTEGER KOBO; conversion NGN -> kobo is
    round-half-up done in decimal space (never float binary rounding).
  * Per-line VAT uses RoundBpsHalfUp (amount_kobo * rate_bps / 10000,
    round-half-up), identical to model.go.
  * IRN = <InvoiceNumber>-<ServiceID8>-<YYYYMMDD> (nrs_irn.go).

The input ``odoo_invoice`` mapping is produced by the Odoo addon
(account.move._nrs_export_dict()) and documented in docs/ERP-ODOO.md.
"""

from __future__ import annotations

import re
from datetime import datetime
from decimal import ROUND_HALF_UP, Decimal

SERVICE_ID_RE = re.compile(r"^[A-Za-z0-9]{8}$")

# NRS tax category catalog (catalogs.go): id -> (rate_bps, canonical S|Z|E)
TAX_CATEGORIES = {
    "STANDARD_VAT": (750, "S"),  # VAT 7.5%
    "ZERO_VAT": (0, "Z"),        # zero-rated (medical services, tuition per NTA 2025 s.187)
    "EXEMPT": (0, "E"),
    "VAT_EXEMPT": (0, "E"),
    "NON_VAT": (0, "E"),
}

INVOICE_TYPE_CODES = {"380", "381", "383", "386", "388", "751"}
TYPE_INVOICE = "380"
TYPE_CREDIT_NOTE = "381"

PAYMENT_STATUSES = {"PENDING", "PAID", "REJECTED"}


class NRSPayloadError(ValueError):
    """Raised when the source invoice cannot be mapped to a valid NRS payload."""

    def __init__(self, errors):
        self.errors = list(errors)
        super().__init__(
            "%d mapping error(s): %s" % (len(self.errors), "; ".join(self.errors))
        )


# ---------------------------------------------------------------------------
# Money helpers (decimal-space round-half-up; mirror nrs_schema.go)
# ---------------------------------------------------------------------------

def ngn_to_kobo(amount) -> int:
    """Decimal NGN amount -> integer kobo, round-half-up in decimal space."""
    d = Decimal(str(amount)).quantize(Decimal("0.01"), rounding=ROUND_HALF_UP)
    return int(d * 100)


def kobo_to_ngn(kobo: int) -> float:
    return kobo / 100.0


def round_bps_half_up(amount_kobo: int, rate_bps: int) -> int:
    """amount_kobo * rate_bps / 10000, round-half-up (mirrors Go model.go)."""
    sign = -1 if amount_kobo < 0 else 1
    n = abs(amount_kobo) * rate_bps
    return sign * ((n + 5000) // 10000)


# ---------------------------------------------------------------------------
# IRN helpers (mirror nrs_irn.go)
# ---------------------------------------------------------------------------

def valid_service_id(service_id: str) -> bool:
    return bool(SERVICE_ID_RE.match(service_id or ""))


def date_stamp(issue_date: str) -> str:
    """YYYY-MM-DD or YYYYMMDD -> validated YYYYMMDD."""
    d = (issue_date or "").strip().replace("-", "")
    if len(d) != 8:
        raise NRSPayloadError(["date stamp %r invalid: want YYYYMMDD" % issue_date])
    try:
        datetime.strptime(d, "%Y%m%d")
    except ValueError as exc:
        raise NRSPayloadError(["date stamp %r invalid: %s" % (issue_date, exc)])
    return d


def build_irn(invoice_number: str, service_id: str, issue_date: str) -> str:
    invoice_number = (invoice_number or "").strip()
    if not invoice_number:
        raise NRSPayloadError(["invoice number is required for IRN"])
    if not valid_service_id(service_id):
        raise NRSPayloadError(
            ["service id %r invalid: must be 8 alphanumeric characters" % service_id]
        )
    return "%s-%s-%s" % (invoice_number, service_id, date_stamp(issue_date))


def parse_irn(irn: str):
    """Split IRN into (invoice_number, service_id, date_stamp); validates."""
    irn = (irn or "").strip()
    parts = irn.rsplit("-", 2)
    if len(parts) != 3 or not all(parts):
        raise NRSPayloadError(
            ["irn %r malformed: want <InvoiceNumber>-<ServiceID>-<YYYYMMDD>" % irn]
        )
    invoice_number, service_id, ds = parts
    if not valid_service_id(service_id):
        raise NRSPayloadError(
            ["irn %r malformed: service id %r must be 8 alphanumeric characters"
             % (irn, service_id)]
        )
    date_stamp(ds)  # raises on invalid date
    return invoice_number, service_id, ds


def valid_irn(irn: str) -> bool:
    try:
        parse_irn(irn)
        return True
    except NRSPayloadError:
        return False


# ---------------------------------------------------------------------------
# Payload construction
# ---------------------------------------------------------------------------

def _party(p: dict, field: str, errors: list, require_tin: bool) -> dict:
    name = (p.get("name") or "").strip()
    tin = (p.get("tin") or "").strip()
    if not name:
        errors.append("%s.party_name is required" % field)
    if require_tin and not tin:
        errors.append("%s.tin is required" % field)
    out = {"party_name": name, "tin": tin}
    if p.get("email"):
        out["email"] = p["email"]
    if p.get("phone"):
        out["telephone"] = p["phone"]
    addr = {}
    for src, dst in (
        ("street", "street_name"),
        ("city", "city_name"),
        ("zip", "postal_zone"),
        ("country", "country"),
        ("state", "state"),
        ("lga", "lga"),
    ):
        if p.get(src):
            addr[dst] = p[src]
    if addr:
        out["postal_address"] = addr
    return out


def build_nrs_invoice(odoo_invoice: dict, service_id: str, business_id: str) -> dict:
    """Map a neutral Odoo invoice dict to the NRS UBL-shaped JSON payload.

    Required keys of ``odoo_invoice``:
        invoice_number, issue_date, move_type ('out_invoice'|'out_refund'),
        supplier {name, tin, ...}, customer {name, tin, ...},
        lines [{name, quantity, price_unit, line_extension_amount,
                tax_category, hsn_code?, description?, discount_rate?}]
    Optional: due_date, currency (default NGN), note, buyer_reference,
        order_reference, payment_means_code, payment_due_date, irn
        (reuse an existing IRN for idempotent resubmission).
    """
    errors = []
    if not valid_service_id(service_id):
        errors.append("service id %r invalid: must be 8 alphanumeric characters" % service_id)
    if not (business_id or "").strip():
        errors.append("business_id is required")

    invoice_number = (odoo_invoice.get("invoice_number") or "").strip()
    issue_date = (odoo_invoice.get("issue_date") or "").strip()
    if not invoice_number:
        errors.append("invoice_number is required")
    if not issue_date:
        errors.append("issue_date is required")

    move_type = odoo_invoice.get("move_type", "out_invoice")
    type_code = TYPE_CREDIT_NOTE if move_type == "out_refund" else TYPE_INVOICE
    if type_code not in INVOICE_TYPE_CODES:
        errors.append("invoice_type_code %r not in catalog" % type_code)

    currency = (odoo_invoice.get("currency") or "NGN").upper()

    lines = odoo_invoice.get("lines") or []
    if not lines:
        errors.append("at least one invoice line is required")

    nrs_lines = []
    # group taxable kobo per NRS tax category for the tax_total subtotals
    groups = {}  # category_id -> [taxable_kobo, tax_kobo]
    total_excl_kobo = 0
    for idx, line in enumerate(lines):
        f = "invoice_line[%d]" % idx
        name = (line.get("name") or "").strip()
        if not name:
            errors.append("%s.item.name is required" % f)
        qty = line.get("quantity", 0)
        try:
            qty = float(qty)
        except (TypeError, ValueError):
            errors.append("%s.invoiced_quantity must be numeric" % f)
            continue
        if qty <= 0:
            errors.append("%s.invoiced_quantity must be > 0" % f)
        price = line.get("price_unit", 0)
        ext = line.get("line_extension_amount")
        if ext is None:
            disc = float(line.get("discount_rate") or 0.0)
            ext = float(price) * qty * (1.0 - disc / 100.0)
        ext_kobo = ngn_to_kobo(ext)
        if ext_kobo < 0:
            errors.append("%s.line_extension_amount must be >= 0" % f)
        price_kobo = ngn_to_kobo(price)
        if price_kobo < 0:
            errors.append("%s.price.price_amount must be >= 0" % f)

        cat = (line.get("tax_category") or "STANDARD_VAT").upper()
        if cat not in TAX_CATEGORIES:
            errors.append("%s.tax_category %r not in catalog" % (f, cat))
            cat = "STANDARD_VAT"
        rate_bps = TAX_CATEGORIES[cat][0]
        tax_kobo = round_bps_half_up(ext_kobo, rate_bps)
        g = groups.setdefault(cat, [0, 0])
        g[0] += ext_kobo
        g[1] += tax_kobo
        total_excl_kobo += ext_kobo

        nrs_line = {
            "invoiced_quantity": qty,
            "line_extension_amount": kobo_to_ngn(ext_kobo),
            "item": {"name": name},
            "price": {"price_amount": kobo_to_ngn(price_kobo), "base_quantity": 1},
        }
        if line.get("description"):
            nrs_line["item"]["description"] = line["description"]
        if line.get("hsn_code"):
            nrs_line["hsn_code"] = line["hsn_code"]
        if line.get("product_category"):
            nrs_line["product_category"] = line["product_category"]
        if line.get("discount_rate"):
            nrs_line["discount_rate"] = float(line["discount_rate"])
        if line.get("sellers_item_identification"):
            nrs_line["item"]["sellers_item_identification"] = line[
                "sellers_item_identification"
            ]
        nrs_lines.append(nrs_line)

    supplier = _party(odoo_invoice.get("supplier") or {},
                      "accounting_supplier_party", errors, require_tin=True)
    customer = _party(odoo_invoice.get("customer") or {},
                      "accounting_customer_party", errors, require_tin=False)
    if errors:
        raise NRSPayloadError(errors)

    total_tax_kobo = sum(g[1] for g in groups.values())
    payable_kobo = total_excl_kobo + total_tax_kobo

    tax_subtotals = []
    for cat, (taxable, tax) in groups.items():
        tax_subtotals.append(
            {
                "taxable_amount": kobo_to_ngn(taxable),
                "tax_amount": kobo_to_ngn(tax),
                "tax_category": {
                    "id": cat,
                    "percent": TAX_CATEGORIES[cat][0] / 100.0,
                },
            }
        )

    payload = {
        "business_id": business_id.strip(),
        "issue_date": issue_date,
        "invoice_type_code": type_code,
        "document_currency_code": currency,
        "invoice_kind": "B2B",
        "payment_status": "PENDING",
        "accounting_supplier_party": supplier,
        "accounting_customer_party": customer,
        "invoice_line": nrs_lines,
        "legal_monetary_total": {
            "line_extension_amount": kobo_to_ngn(total_excl_kobo),
            "tax_exclusive_amount": kobo_to_ngn(total_excl_kobo),
            "tax_inclusive_amount": kobo_to_ngn(payable_kobo),
            "payable_amount": kobo_to_ngn(payable_kobo),
        },
    }
    if errors:
        raise NRSPayloadError(errors)

    # IRN: reuse a client-supplied one (idempotent resubmission) or build it.
    irn = odoo_invoice.get("irn")
    if irn:
        if not valid_irn(irn):
            raise NRSPayloadError(
                ["irn must be <InvoiceNumber>-<ServiceID>-<YYYYMMDD>"]
            )
        payload["irn"] = irn.strip()
    else:
        payload["irn"] = build_irn(invoice_number, service_id, issue_date)
    payload["buyer_reference"] = odoo_invoice.get("buyer_reference") or invoice_number

    if tax_subtotals:
        payload["tax_total"] = [
            {"tax_amount": kobo_to_ngn(total_tax_kobo), "tax_subtotal": tax_subtotals}
        ]
    if odoo_invoice.get("due_date"):
        payload["due_date"] = odoo_invoice["due_date"]
    if odoo_invoice.get("note"):
        payload["note"] = odoo_invoice["note"]
    if odoo_invoice.get("order_reference"):
        payload["order_reference"] = odoo_invoice["order_reference"]
    if odoo_invoice.get("payment_means_code"):
        pm = {"payment_means_code": odoo_invoice["payment_means_code"]}
        if odoo_invoice.get("payment_due_date") or odoo_invoice.get("due_date"):
            pm["payment_due_date"] = odoo_invoice.get("payment_due_date") or odoo_invoice[
                "due_date"
            ]
        payload["payment_means"] = [pm]
    return payload
