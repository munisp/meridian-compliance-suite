# meridian-compliance-suite

**Meridian Compliance Suite: e-invoicing (T1/T2/T3), POS-VAT (T6), WHT (T7), TP/CbCR (T8), ETR (T9), CARF/VASP (T10), practitioner case mgmt (T13).**

## Purpose
Taxpayer- and practitioner-facing compliance rails: continuous transaction controls (e-invoicing tiers), POS VAT capture, withholding tax, transfer pricing / CbCR, effective tax rate computation, CARF/VASP reporting, and practitioner case management.

## Plane mapping
- **Execution plane:** e-invoicing clearance, POS-VAT capture, WHT flows (T1/T2/T3/T6/T7)
- **Data plane:** ETR computation, TP/CbCR data collection (T8/T9)
- **Control plane:** CARF/VASP reporting, practitioner case mgmt (T10/T13)
- All tax logic sourced from signed packs in `meridian-rule-packs`; platform services from `meridian-core-platform`.

## Sibling repositories
- [meridian-core-platform](https://github.com/munisp/meridian-core-platform)
- [meridian-inclusion-suite](https://github.com/munisp/meridian-inclusion-suite)
- [meridian-gov-enclave](https://github.com/munisp/meridian-gov-enclave)
- [meridian-rule-packs](https://github.com/munisp/meridian-rule-packs)
- [meridian-docs](https://github.com/munisp/meridian-docs)

**Status:** scaffold
