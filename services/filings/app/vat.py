"""F1 — VAT return (Form VAT 002) model and assembly.

Monthly return due the 21st of the following month (nil returns mandatory).
Computation: net VAT = output VAT - deductible input VAT +/- adjustments.
Zero-rated supplies retain input-VAT recovery; exempt supplies do not
(non-deductible input VAT attributable to exempt sales is excluded).

Assembly: accepts cleared e-invoice rows (NRS MBS shape: irn, basket,
net_kobo, vat_kobo, direction sale/purchase, doc_kind invoice/credit_note/
debit_note) where available; also accepts raw schedule totals.

Sources: NTAA 2025 s.22; Stripe Nigeria VAT guide; finorabusiness VAT-002
workbook guide. Integer kobo, round-half-up.

REAL: return model, netting, schedules, deadline validation, amendment
idempotency. SIM: e-invoice feed is caller-supplied rows (no live MBS
subscription in this service).
"""
from __future__ import annotations

import itertools
from datetime import date

from . import store
from .rules_data import VAT_RATE_BPS, resolve
from .util import deadline_nth_of_following_month, parse_period

VAT_FILING_DAY = 21  # 21st of following month (rp-fmt-federal fmt.federal.vat)

DEDUCTIBLE_DOC_KINDS = ("invoice", "debit_note")   # purchases side
ADJUSTMENT_KINDS = ("credit_note", "bad_debt_relief", "reverse_charge")

_ids = itertools.count(1)


class VatError(ValueError):
    pass


def _sale_rows(invoices):
    return [i for i in invoices if i.get("direction", "sale") == "sale"]


def _purchase_rows(invoices):
    return [i for i in invoices if i.get("direction") == "purchase"]


def build_return(tin: str, period: str, invoices: list[dict] | None = None,
                 sales_schedule: dict | None = None,
                 purchases: list[dict] | None = None,
                 adjustments: list[dict] | None = None,
                 exempt_input_share_bps: int = 0) -> dict:
    """Assemble a Form VAT 002 return. Integer kobo in/out.

    exempt_input_share_bps: share of generic input VAT attributable to
    exempt sales (non-deductible exclusion, NTAA s.22 / zero-rated-vs-exempt
    semantics). Input VAT lines already tagged exempt_attributable are
    excluded in full.
    """
    parse_period(period)  # validate
    year, month = parse_period(period)
    eff = date(year, month, 1)
    resolve(VAT_RATE_BPS, eff)  # fail-closed era check

    invoices = invoices or []
    sales = _sale_rows(invoices)
    purchases = list(purchases or []) + _purchase_rows(invoices)
    adjustments = adjustments or []

    std_sales = sum(int(i["net_kobo"]) for i in sales if i.get("basket") == "standard_75")
    zero_sales = sum(int(i["net_kobo"]) for i in sales if i.get("basket") == "zero_rated")
    exempt_sales = sum(int(i["net_kobo"]) for i in sales if i.get("basket") == "exempt")
    if sales_schedule:  # raw schedule path (no e-invoice feed)
        std_sales = int(sales_schedule.get("standard_sales_kobo", std_sales))
        zero_sales = int(sales_schedule.get("zero_rated_sales_kobo", zero_sales))
        exempt_sales = int(sales_schedule.get("exempt_sales_kobo", exempt_sales))
    output_vat = sum(int(i.get("vat_kobo", 0)) for i in sales)
    if sales_schedule and "output_vat_kobo" in sales_schedule:
        output_vat = int(sales_schedule["output_vat_kobo"])

    gross_input = sum(int(p.get("vat_kobo", 0)) for p in purchases
                      if p.get("doc_kind", "invoice") in DEDUCTIBLE_DOC_KINDS)
    # non-deductible exclusions: input VAT attributable to exempt supplies
    exempt_attributable = sum(int(p.get("vat_kobo", 0)) for p in purchases
                              if p.get("exempt_attributable"))
    generic_share = (gross_input - exempt_attributable) * int(exempt_input_share_bps) // 10_000
    non_deductible = exempt_attributable + generic_share
    input_vat = gross_input - non_deductible

    adj_total = 0
    adj_lines = []
    for a in adjustments:
        kind = a.get("kind")
        if kind not in ADJUSTMENT_KINDS:
            raise VatError(f"unknown adjustment kind {kind!r}")
        amt = int(a["vat_kobo"])
        signed = -amt if kind == "credit_note" else amt  # credit notes reduce output VAT
        adj_total += signed
        adj_lines.append({"kind": kind, "vat_kobo": amt, "signed_vat_kobo": signed,
                          "ref": a.get("ref", "")})

    net = output_vat - input_vat + adj_total
    return {
        "form": "VAT-002",
        "tin": tin,
        "period": period,
        "deadline": deadline_nth_of_following_month(period, VAT_FILING_DAY).isoformat(),
        "nil_return": (std_sales + zero_sales + exempt_sales == 0 and gross_input == 0
                       and not adjustments),
        "sales_schedule": {
            "standard_sales_kobo": std_sales,
            "zero_rated_sales_kobo": zero_sales,
            "exempt_sales_kobo": exempt_sales,
            "total_sales_kobo": std_sales + zero_sales + exempt_sales,
        },
        "output_vat_kobo": output_vat,
        "input_vat": {
            "gross_input_vat_kobo": gross_input,
            "non_deductible_exclusions_kobo": non_deductible,
            "deductible_input_vat_kobo": input_vat,
        },
        "adjustments": adj_lines,
        "adjustments_vat_kobo": adj_total,
        "net_vat_payable_kobo": net if net >= 0 else 0,
        "refund_kobo": -net if net < 0 else 0,
        "source": "einvoice+schedules" if invoices else "schedules",
    }


class VatReturnStore:
    """Filing store: one live return per (tin, period); amendments supersede
    and are idempotent per amendment idempotency key.

    REAL: durable via app.store.DocStore — Postgres when DATABASE_URL /
    FILINGS_DATABASE_URL is set (prod), in-memory fallback in dev.
    """

    def __init__(self, docs: "store.DocStore | None" = None):
        self._docs = docs if docs is not None else store.DocStore()
        # re-seed the ID counter so restart on a durable backend cannot
        # re-issue an existing return_id
        store.seed_counter(_ids, store.max_id_suffix(
            self._docs, "vat_returns", "return_id", "VAT002-"))

    @staticmethod
    def _key(tin: str, period: str) -> str:
        return f"{tin}|{period}"

    def file(self, ret: dict, idempotency_key: str,
             amendment_of: str | None = None) -> tuple[dict, bool]:
        replay = self._docs.get("vat_idem", idempotency_key)
        if replay is not None:
            return replay, False  # replay
        prior = self._docs.get("vat_returns", self._key(ret["tin"], ret["period"]))
        version = 1 if prior is None else prior["version"] + 1
        if prior is not None and amendment_of is None:
            raise VatError("return already filed for period; submit as amendment")
        rec = dict(ret)
        rec.update({
            "return_id": f"VAT002-{next(_ids):06d}",
            "version": version,
            "status": "amended" if version > 1 else "filed",
            "amends": prior["return_id"] if prior else None,
            "filed_at": date.today().isoformat(),
        })
        self._docs.put("vat_returns", self._key(ret["tin"], ret["period"]), rec)
        self._docs.put("vat_idem", idempotency_key, rec)
        return rec, True

    def get(self, tin: str, period: str) -> dict | None:
        return self._docs.get("vat_returns", self._key(tin, period))
