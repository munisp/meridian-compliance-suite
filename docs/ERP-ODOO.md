# ERP-ODOO — Meridian NRS e-Invoicing Integration for Odoo

Native Odoo integration that clears customer invoices with the Nigerian
Revenue Service (NRS) e-invoicing regime through the Meridian `einvoicing`
service (`services/einvoicing` in this repo).

## REAL / SIM statement (read first)

| Component | Status |
|---|---|
| `meridian_odoo_client` (mapping, IRN, kobo rounding, HMAC, lifecycle) | **REAL, unit-tested** — 41 pytest tests, all green, no Odoo required |
| `meridian_nrs_einvoice` Odoo addon (models/views/controller) | **REAL code, NOT executed against a live Odoo in CI** — covered by an 18-test structural harness (manifest parse, security CSV, XML lint, compile, route decorators, golden export-dict payloads); requires a live Odoo 17/18 host for runtime UAT. Import-guarded: the addon loads but raises a clear `UserError` on submission if `meridian_odoo_client` is not on the Odoo host PYTHONPATH |
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

## Odoo 17/18 compatibility

The addon supports **Odoo 17 and Odoo 18** with a single codebase:

- **Manifest version prefix**: Odoo refuses to install a module whose
  `version` does not start with the running series. The shipped manifest
  is `17.0.1.1.0`; for an Odoo 18 deployment change **only** the prefix to
  `18.0.1.1.0` (see the header comment in `__manifest__.py`). No other
  manifest key differs between series. The structural harness accepts both
  prefixes so a renamed manifest stays green.
- **Cron records**: the legacy `numbercall = -1` ("run forever") idiom is
  removed. `numbercall` is omitted entirely, which Odoo 17 and 18 both
  treat as unlimited; `doall` is left at its default (`False`) so missed
  runs are not bulk-replayed after downtime. A harness test fails if
  `numbercall` reappears in `data/ir_cron.xml`.
- **Settings view anchors**: `views/res_config_settings_views.xml` splits
  the inherit into a version-sensitive primary anchor
  (`//app[@name='account']` on `account.res_config_settings_view_form`)
  and a build-agnostic secondary xpath that fills the inserted block via
  the `//setting[...]` structure. Odoo 17.x builds that render the
  settings form as flat `//setting` groups only need the **first** xpath
  repointed (documented in the file header comment); never enable two
  anchors at once (double insert).
- **View syntax**: all views use the post-17 inline `invisible="..."`
  expression syntax (the pre-17 `attrs` dict was removed in Odoo 18) and
  the `<list>` root tag (`<tree>` was removed in Odoo 18). Fallback anchors
  for renamed search filters are documented in
  `views/account_move_views.xml`.
- **Python API**: the addon uses only APIs stable across 17/18
  (`fields`, `api.model` crons, `http.route(type='http', auth='none',
  csrf=False)`, `ir.config_parameter`, `tracking=True`). No version-gated
  imports.

## What remains for live-Odoo UAT (exact checklist)

Everything below **requires a running Odoo host** and cannot be simulated
in CI; each item is the exact verification to perform:

1. **Install on Odoo 17**: module installs cleanly; settings panel renders
   under Settings → Invoicing → *Meridian NRS e-Invoicing*; both cron jobs
   appear under Settings → Technical → Automation → Scheduled Actions with
   "Number of Calls" empty (unlimited).
2. **Install on Odoo 18**: same checks with the `18.0.` manifest prefix;
   confirm the settings block lands inside the Invoicing app container
   (if the build renders flat settings, apply the documented xpath
   fallback and re-verify).
3. **Smoke invoice**: post a customer invoice (VAT 7.5%) with
   `auto_submit` on → IRN `INV…-SERVICEID-YYYYMMDD` stored,
   `nrs_clearance_status` advances, QR payload populated; payload visible
   in *NRS Submission Queue*.
4. **Webhook round-trip**: register the webhook against the service,
   confirm a signed `transmitted`/`confirmed` delivery advances the
   status, a tampered body is rejected 401, and an unknown IRN returns
   200/ignored without error.
5. **Credit note**: post + submit an `out_refund`; service receives
   `invoice_type_code = 381` with positive amounts and its own IRN.
6. **Error queue**: force a failure (stop the service), watch the log row
   requeue with 15-min linear backoff, restart the service, confirm the
   queue cron drains it within 5 attempts.
7. **Payment sync**: pay a cleared invoice; within one cron hour the
   service shows `payment_status=PAID` for that IRN.
8. **Access control**: as a Billing user (no manager group), the queue is
   read/write but not deletable; as a non-accounting user the NRS tab and
   menu are hidden.
9. **Multi-company** (if used): parameters are database-global — confirm
   this is acceptable or duplicate the addon per DB before go-live.

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
python3 -m pytest meridian_odoo_client/tests tools -q   # 59 tests
```

Covers: Odoo-invoice-dict → NRS payload round-trip validated against the Go
schema expectations (IRN format incl. hyphenated numbers, kobo round-half-up
edge cases like 0.575→58, VAT 7.5% grouping, zero-rated categories, credit
note 381, idempotent IRN reuse), client calls with mocked transport (auth
header, 422 error aggregation, PATCH by IRN, webhook registration), webhook
HMAC accept/reject (wrong secret, tampered body, empty inputs), and
lifecycle transition legality (happy path, fail→retry, illegal jumps).

Mapper edge cases (`test_mapper_edge_cases.py`): x.xx5 rounding boundary
sweep (incl. float-hostile 2.675→268 kobo and string inputs), bps-rounding
boundaries, missing/blank supplier TIN → aggregated clear errors, customer
TIN optional (B2C), >99-line exports (150/101 lines, no pagination
truncation), credit-note sign handling (positive amounts, negative amounts
rejected), foreign currency rejected with a clear error (NRS clearance is
NGN-only), `ngn` case-insensitive acceptance.

Addon structural harness (`tools/test_addon_structure.py`, 18 tests, no
Odoo runtime): manifest parses via `ast.literal_eval` with a 17.0/18.0
version prefix and existing data files; security CSV header/row lint; all
XML well-formed with unique record ids and non-empty inherit_id refs and
xpath exprs; cron XML free of the deprecated `numbercall` idiom; all addon
Python compiles; webhook `@http.route` decorators carry
`type/auth='none'/methods=['POST']/csrf=False`; and three golden
`_nrs_export_dict` payloads map correctly through `build_nrs_invoice`
(multi-tax-group, zero-rated, foreign-currency rejection).
