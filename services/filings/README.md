# filings — periodic filing layer

FastAPI service for statutory periodic filings that previously existed only
as deadlines in the filing-matrix pack (parity4/gov-filing-gaps F1-F4).
Errors are RFC 7807 (`application/problem+json`). All monetary values are
integer kobo; rate math rounds half-up (`app/util.kobo_mul`).

## Modules & honesty tags

| Module | Scope | Tag |
|---|---|---|
| `app/vat.py` | Form VAT 002: output/input VAT netting, exempt vs zero-rated schedules, non-deductible input exclusions, adjustments (credit/debit notes, bad-debt relief, reverse charge), nil returns, 21st-of-following-month deadline, amendment returns with same-period idempotency | REAL |
| `app/paye.py` | Monthly PAYE schedule (employee rows: TIN/name/gross/pension/reliefs/tax), 10th deadline (PITA s.81), annual Form H1 aggregation due 31 Jan; bands/CRA from rp-paye-pitra-legacy values | REAL |
| `app/cit.py` | CIT chain: assessable profit -> capital allowance -> loss relief (4-yr pre-NTA cap, effective-dated; unlimited under NTA 2025) -> total profit -> tiered CIT -> minimum-tax floor (small co exempt) -> 4% development levy -> effective tax payable. Pillar Two top-up (etr) applies AFTER this figure | REAL |
| `app/assessment.py` | Additional/best-of-judgment assessment + demand notice with s.40 service metadata; final-and-conclusive lapse; s.41 objection (30-day clock, grounds + admitted-amount validation, partial payment of admitted amount); 90-day decision window with deemed-upheld default and TAT referral record | REAL |
| e-invoice feed into `vat.build_return` | caller-supplied cleared-invoice rows; no live MBS subscription here | SIM |
| service-of-notice / partial payment | recorded metadata / ledger entries, not actual dispatch or payment rails | SIM |

## Rules as data

`app/rules_data.py` holds effective-dated rate tables (VAT 7.5%, CIT tiers
and small-company thresholds, capital allowance IA/AA, loss-relief cap,
minimum-tax 0.5%, dev levy 4%, PAYE CRA/bands). **Rule-pack sync
requirement:** these mirror meridian-rule-packs semantics; when packs are
published for these computations this module must load them (drift-guarded
like `services/pos-vat/embedded_packs.go`).

## API sketch

- `POST /v1/filings/vat` (idempotent by `idempotency_key`; amendments via `amendment_of`), `GET /v1/filings/vat/{tin}/{period}`
- `POST /v1/filings/paye`, `POST /v1/filings/paye/h1?employer_tin&year`
- `POST /v1/filings/cit/compute`
- `POST /v1/assessments`, `GET /v1/assessments/{id}`, `POST /v1/assessments/{id}/objections`, `POST /v1/objections/{id}/decision`, `POST /v1/assessments/tick?today=`, `GET /v1/assessments/tat-referrals`
- `GET /v1/exports/taxpromax.csv?tin=&from_period=&to_period=&tax_type=` — I2 TaxProMax CSV export of filed returns for accountants; authenticated + TIN-scoped (taxpayer own-TIN), streams `text/csv`, audit-logged per export. Column format is documented in `app/taxpromax.py` (naira 2dp; the nactp reference export lacked TIN/tax-type columns, so a clean format was defined rather than copied)

## Tests

`pip install -r requirements.txt && python3 -m pytest -q` (20 tests).
