# ERP-ODOO — Meridian NRS e-Invoicing Integration for Odoo

Native Odoo integration that clears customer invoices with the Nigerian
Revenue Service (NRS) e-invoicing regime through the Meridian `einvoicing`
service (`services/einvoicing` in this repo).

## REAL / SIM statement (read first)

| Component | Status |
|---|---|
| `meridian_odoo_client` (mapping, IRN, kobo rounding, HMAC, lifecycle) | **REAL, unit-tested** — 25 pytest tests, all green, no Odoo required |
| `meridian_nrs_einvoice` Odoo addon (models/views/controller) | **REAL code, NOT executed in CI** — requires a live Odoo 17/18 host; syntax-compiled and XML-validated here. Import-guarded: the addon loads but raises a clear `UserError` on submission if `meridian_odoo_client` is not on the Odoo host PYTHONPATH |
| End-to-end clearance against an NRS sandbox | **REQUIRES LIVE SYSTEMS** — needs a running Meridian einvoicing service (or NRS sandbox credentials) reachable from the Odoo host. Not simulated |
| `docker-compose.odoo.yml` | UAT convenience only; optional |

Nothing in this integration fabricates a clearance: invoices only reach
`confirmed` when the Meridian service's 8-step lifecycle confirms them.

## Architecture

```
Odoo 17/18                                        Meridian einvoicing svc
┌─────────────────────────────┐  POST /v1/invoices/nrs  ┌───────────────────┐
│ account.move (addon)        │ ───────────────────────▶│ 8-step lifecycle: │
│  action_post / button       │   NRS UBL-shaped JSON   │ create, IRN gen/  │
│  _nrs_export_dict()         │ ◀───────────────────────│ validate, IRN- &  │
│        │                    │   irn, status, QR, stamp│ invoice-sign,     │
│        ▼                    │                         │ transmit, confirm │
│ meridian_odoo_client        │  PATCH /v1/invoices/{irn}└───────┬───────────┘
│  build_nrs_invoice()        │ ───────────────────────▶        │
│  (kobo-int, half-up)        │  payment_status=PAID            │ HMAC webhook
│ cron: queue retry +         │                                 ▼
│  payment sync               │  POST /meridian_nrs/webhook  ┌──────────────┐
│ controller: HMAC verify     │ ◀────────────────────────────│ X-Meridian-  │
│  → _nrs_handle_webhook_event│  X-Meridian-Signature        │ Signature    │
└─────────────────────────────┘                              └──────────────┘
```

- **IRN** = `<InvoiceNumber>-<ServiceID8>-<YYYYMMDD>` (e.g.
  `INV20260001-94ND90NR-20260127`). Odoo invoice numbers have `/` stripped
  (`INV/2026/0001` → `INV20260001`).
- **Money**: Odoo NGN floats are converted to **integer kobo with
  round-half-up in decimal space** inside `meridian_odoo_client.schema`
  (mirrors `NGNToKobo`/`RoundBpsHalfUp` in the Go service). VAT is computed
  per tax category from kobo; floats only appear at the JSON boundary.
- **Lifecycle on the Odoo side**: `nrs_clearance_status` mirrors the
  service's 8 steps coarsely: `draft → submitted → signed → transmitted →
  confirmed`, plus `failed` (with `nrs_last_error`). Webhooks only move the
  status forward; out-of-order deliveries are ignored.
- **Error queue**: every attempt is a `nrs.submission.log` row. Failures
  requeue with 15-minute linear backoff, max 5 attempts; the 15-min cron
  retries them. Manual retry from the log list/form.
- **Credit notes**: `out_refund` maps to NRS `invoice_type_code = 381` with
  positive amounts, own IRN, same lifecycle.
- **Payment status**: hourly cron PATCHes `payment_status=PAID` by IRN for
  invoices whose Odoo `payment_state` became paid/in_payment (payment status
  is the only mutable field after the service's signage step).

## Field mapping (Odoo → NRS schema)

| Odoo (`account.move` / related) | NRS payload field | Notes |
|---|---|---|
| `name` (slashes stripped) | `irn` number part + `buyer_reference` | IRN built as `number-serviceID-YYYYMMDD` |
| `invoice_date` | `issue_date` (+ IRN date stamp) | |
| `invoice_date_due` | `due_date`, `payment_means[].payment_due_date` | |
| `move_type` | `invoice_type_code` | `out_invoice`→380, `out_refund`→381 |
| `currency_id.name` | `document_currency_code` | default NGN |
| `company_id.partner_id.name/.vat/.email/.phone/.street/.city/.zip/.state_id.code/.country_id.code` | `accounting_supplier_party` | **`vat` = Nigerian TIN (required)** |
| `partner_id` same fields | `accounting_customer_party` | TIN optional for B2C |
| `invoice_line_ids.name` | `invoice_line[].item.name` | |
| `invoice_line_ids.product_id.description_sale` | `item.description` | |
| `product_id.default_code` | `item.sellers_item_identification` | |
| `product_id.hs_code` | `invoice_line[].hsn_code` | |
| `quantity` / `price_unit` | `invoiced_quantity` / `price.price_amount` | |
| `discount` | `discount_rate` | |
| `price_subtotal` | `line_extension_amount` | tax-exclusive line total |
| `tax_ids[:1].nrs_tax_category` (added by addon) | `tax_total[].tax_subtotal[].tax_category` | STANDARD_VAT 7.5% / ZERO_VAT (medical, tuition per NTA 2025 s.187) / EXEMPT / NON_VAT |
| computed kobo sums | `legal_monetary_total` | half-up rounding |
| `narration` / `ref` | `note` / `order_reference` | |
| — | `payment_means_code` | fixed `30` (credit transfer) |
| — | `business_id` | from settings |

## Setup guide

1. **Install the client package** on the Odoo host:
   `pip install integrations/odoo/meridian_odoo_client` (or add it to
   PYTHONPATH, as the compose file does).
2. **Install the addon**: copy/symlink `integrations/odoo/meridian_nrs_einvoice`
   into the Odoo addons path, update the app list, install
   *Meridian NRS e-Invoicing*.
3. **Configure** Settings → Invoicing → *Meridian NRS e-Invoicing*:
   - Base URL of the Meridian einvoicing service, API key (Bearer),
   - NRS **service id** (8 chars, issued at NRS integrator onboarding),
   - Meridian **business id**, webhook secret (≥16 chars).
4. **Register the webhook** with the service:
   `POST /v1/webhooks {"business_id": ..., "url":
   "https://<odoo-host>/meridian_nrs/webhook", "secret": <same secret>}`
   (use `MeridianClient.register_webhook()`).
5. **Tax mapping**: on each `account.tax`, set *NRS Tax Category*. Nigerian
   VAT 7.5% → STANDARD_VAT; medical services and tuition taxes → ZERO_VAT
   (zero-rated under NTA 2025 s.187, not exempt — input VAT stays
   recoverable); exempt/out-of-scope → EXEMPT/NON_VAT.
6. **Partner data**: every invoicing company partner must carry its TIN in
   the `VAT` field (NRS requires supplier TIN). Customer TIN recommended for
   B2B.
7. Post an invoice → it is submitted automatically (or use the **Submit to
   NRS** button). Track the *NRS e-Invoicing* tab on the invoice and
   *Invoicing → NRS Submission Queue* for failures.

For local UAT: `docker compose -f integrations/odoo/docker-compose.odoo.yml
up -d` (Odoo 17 on :8069 with the addon and client mounted).

## SME go-live checklist

- [ ] NRS integrator onboarding complete; production service id received
- [ ] Meridian einvoicing service deployed with production signer (keyx) —
      not the dev auto-assign registry
- [ ] Webhook registered over HTTPS with ≥16-byte secret; signature reject
      tested with a tampered body
- [ ] Company TIN set; customer TIN capture added to sales workflow (B2B)
- [ ] Tax categories mapped (VAT 7.5% standard; medical/tuition zero-rated)
- [ ] Test invoice + credit note cleared end-to-end; QR payload verified
- [ ] Error queue empty; retry cron active; alerting on `failed` status
- [ ] Payment-status sync verified (pay an invoice, see PAID by IRN)
- [ ] Backup/rollback plan: disable `auto_submit` before disabling module

## Testing

```bash
cd integrations/odoo
python3 -m pytest meridian_odoo_client/tests -q   # 25 tests
```

Covers: Odoo-invoice-dict → NRS payload round-trip validated against the Go
schema expectations (IRN format incl. hyphenated numbers, kobo round-half-up
edge cases like 0.575→58, VAT 7.5% grouping, zero-rated categories, credit
note 381, idempotent IRN reuse), client calls with mocked transport (auth
header, 422 error aggregation, PATCH by IRN, webhook registration), webhook
HMAC accept/reject (wrong secret, tampered body, empty inputs), and
lifecycle transition legality (happy path, fail→retry, illegal jumps).
