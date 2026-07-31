package main

// LCE SPEC §5 runtime citation chain — einvoicing response layer.
//
// Every computed VAT amount (invoice line vat_amount_kobo) carries its
// statute citation: standard-rated lines cite the VAT Act 7.5% row (coverage:
// vat-act-legacy rate.7.5pct-from-2020-02-01, citation_kind primary);
// zero-rated lines cite the zero-rate row (medical/tuition post-2026 map to
// NTA 2025 s.187 in the coverage matrix; line-level category detail lives in
// pos-vat). Additive, omitempty; resolution is a lookup keyed on the line's
// VatCategory — no change to VAT computation, integer kobo untouched.

import (
	"sync"

	"github.com/munisp/meridian-compliance-suite/packages/shared/rulepack"
)

var (
	citationOnce     sync.Once
	citationResolver *rulepack.CitationResolver
)

func citations() *rulepack.CitationResolver {
	citationOnce.Do(func() {
		// "" = $LCE_COVERAGE_DIR or the vendored embedded coverage matrix.
		citationResolver = rulepack.NewCitationResolver("")
	})
	return citationResolver
}

// vatRuleByCategory maps the UBL/MBS VAT category to the rp-vat-rates rule
// that priced the line.
var vatRuleByCategory = map[string]struct {
	ruleID   string
	citation string // fallback text when the coverage matrix has no row
}{
	"S": {"vat.rate.standard", "VAT Act (as amended by Finance Act 2019), standard rate 7.5%"},
	"Z": {"vat.rate.zero", "Nigeria Tax Act 2025 s.187, zero-rated supplies (0%; input VAT recoverable)"},
	"E": {"vat.rate.exempt", "VAT Act, First Schedule — exempt supplies (no input VAT recovery)"},
}

// attachCitations annotates each invoice line with the statute citation for
// its computed vat_amount_kobo. Total function: unknown categories or a
// missing coverage matrix degrade to empty statute fields, never an error.
func attachCitations(inv *CanonicalInvoice) {
	for i := range inv.Lines {
		l := &inv.Lines[i]
		rule, ok := vatRuleByCategory[l.VatCategory]
		if !ok {
			continue
		}
		l.Citations = []rulepack.Citation{citations().Resolve(
			"rp-vat-rates", "1.0.0", rule.ruleID, rule.citation)}
	}
}
