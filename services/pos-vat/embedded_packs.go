package main

import "encoding/json"

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// Embedded fallback copies of the rp-vat-* packs (pinned core contracts v1).
// Used when RP_REGISTRY_URL is unset or the registry is unreachable (SPEC §1.3
// offline-resilience). Content mirrors meridian-rule-packs v1.0.0.
var embeddedPacks = map[string]string{
	"rp-vat-rates": `id: rp-vat-rates
version: 1.0.0
effective_from: 2025-01-01
effective_to: null
status: published
subject_to_regazette: true
provenance:
  as_passed: "Nigeria Tax Act 2025: VAT retained at 7.5% (s.146)"
  as_gazetted: null
  source_citation: "Nigeria Tax Act 2025 (VAT provisions); Finance Act 2019 s.33"
rules:
  - id: vat.rate.standard
    when: { basket: standard_75 }
    then: { rate_bps: 750, narrate: "VAT 7.5% standard-rated supplies" }
  - id: vat.rate.zero
    when: { basket: zero_rated }
    then: { rate_bps: 0, narrate: "VAT 0% zero-rated supplies (input VAT recoverable)" }
  - id: vat.rate.exempt
    when: { basket: exempt }
    then: { rate_bps: 0, narrate: "Exempt supplies (no input VAT recovery)" }
`,
	"rp-vat-exempt-basket": `id: rp-vat-exempt-basket
version: 1.0.0
effective_from: 2025-01-01
effective_to: null
status: published
subject_to_regazette: true
provenance:
  as_passed: "Nigeria Tax Act 2025, First Schedule (exempt goods and services)"
  as_gazetted: null
  source_citation: "Nigeria Tax Act 2025 First Schedule"
rules:
  - id: vat.exempt.financial
    when: {}
    then: { categories: [financial_services, insurance_premium, interest], narrate: "Financial services exempt" }
  - id: vat.exempt.land
    when: {}
    then: { categories: [land_sale, residential_rent, real_property], narrate: "Land and residential rent exempt" }
  - id: vat.exempt.public
    when: {}
    then: { categories: [postage, government_service, embassy_supply], narrate: "Public/government supplies exempt" }
  - id: vat.exempt.transport
    when: {}
    then: { categories: [public_transport, shared_transport], narrate: "Shared passenger transport exempt" }
`,
	"rp-vat-zerorated-basket": `id: rp-vat-zerorated-basket
version: 1.0.0
effective_from: 2025-01-01
effective_to: null
status: published
subject_to_regazette: true
provenance:
  as_passed: "Nigeria Tax Act 2025, Second Schedule (zero-rated: food, health, education)"
  as_gazetted: null
  source_citation: "Nigeria Tax Act 2025 Second Schedule"
rules:
  - id: vat.zero.food
    when: {}
    then: { categories: [basic_food, groceries, bread, water_sachet, agriculture_input], narrate: "Basic food items zero-rated" }
  - id: vat.zero.health
    when: {}
    then: { categories: [medical, pharmacy, healthcare, hospital_service], narrate: "Healthcare zero-rated" }
  - id: vat.zero.education
    when: {}
    then: { categories: [education, books, tuition, school_materials], narrate: "Education zero-rated" }
  - id: vat.zero.energy
    when: {}
    then: { categories: [cooking_gas, kerosene_household, renewable_equipment], narrate: "Household energy zero-rated" }
  - id: vat.zero.exports
    when: {}
    then: { categories: [export_goods, export_services], narrate: "Exports zero-rated" }
`,
	"rp-vat-attribution-mode": `id: rp-vat-attribution-mode
version: 1.0.0
effective_from: 2025-01-01
effective_to: null
status: published
subject_to_regazette: true
provenance:
  as_passed: "NTAA 2025 s.77: VAT sharing FG 10% / States 55% / LGAs 35%; 30% place-of-consumption derivation"
  as_gazetted: null
  source_citation: "Nigeria Tax Administration Act 2025 (VAT attribution)"
rules:
  - id: vat.attribution.mode
    when: {}
    then: { mode: state, narrate: "state = place-of-consumption; federal = pool; dual_shadow = compute both" }
  - id: vat.attribution.shares
    when: {}
    then: { federal_share_bps: 1000, state_share_bps: 5500, lga_share_bps: 3500, narrate: "NTAA 2025 vertical sharing formula" }
  - id: vat.attribution.derivation
    when: {}
    then: { consumption_weight_bps: 3000, narrate: "30% of state share by place of consumption" }
`,
	"rp-platform-collectors": `id: rp-platform-collectors
version: 1.0.0
effective_from: 2025-01-01
effective_to: null
status: published
subject_to_regazette: true
provenance:
  as_passed: "Designated digital platform VAT collection regime (Market Zone collectors)"
  as_gazetted: null
  source_citation: "NRS designation notice: platform VAT collectors 2025"
rules:
  - id: vat.platform.collectors
    when: {}
    then: { tin_prefixes: [PLTC, MRKT, ECOM], narrate: "Designated platform collector TIN prefixes" }
`,
}
