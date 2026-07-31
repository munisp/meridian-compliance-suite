package main

// LCE SPEC §5 runtime citation chain — pos-vat response layer.
//
// Every computed VAT amount carries its statute citations: the standard rate
// cites the VAT Act (coverage: vat-act-legacy rate.7.5pct-from-2020-02-01,
// citation_kind primary); zero-rated medical/tuition post-2026 cites the
// Nigeria Tax Act 2025 s.187 (coverage: nta-2025 s.187.zero-rated-*). Fields
// are additive (omitempty); resolution is a lookup over basket decisions
// already made in processReceipt — no computation change, integer kobo
// untouched. citation_kind is honest per the coverage matrix (secondary rows
// pending CTC verification, owned by the registry workstream).

import (
	"sort"
	"strings"
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

// zeroRatedCoverageRule maps embedded zero-basket categories to the canonical
// coverage-matrix rule ids (nta-2025 s.187). Categories not listed resolve
// through the embedded pack rule with no coverage hit (tolerated: statute
// fields empty, citation text falls back to the pack provenance).
var zeroRatedCoverageRule = map[string]string{
	"medical":          "vat.zero.medical-services",
	"pharmacy":         "vat.zero.medical-services",
	"healthcare":       "vat.zero.medical-services",
	"hospital_service": "vat.zero.medical-services",
	"education":        "vat.zero.education-tuition",
	"tuition":          "vat.zero.education-tuition",
	"books":            "vat.zero.education-tuition",
	"school_materials": "vat.zero.education-tuition",
}

// packSourceCitation returns the pack's provenance.source_citation ("" if
// absent) — used as fallback citation text for rules without a coverage hit.
func packSourceCitation(p *Pack) string {
	if p == nil {
		return ""
	}
	return strOf(p.Provenance["source_citation"])
}

// receiptCitations builds the citation set for a processed receipt from the
// basket decisions already taken (rate rule + basket decision rule per SPEC
// §5.1). Deterministic order: standard rate, zero-rated rules (sorted),
// exempt basket.
func (s *Service) receiptCitations(basketCats map[string]map[string]bool) []rulepack.Citation {
	var out []rulepack.Citation
	if len(basketCats["standard_75"]) > 0 {
		p := s.packs.Get("rp-vat-rates")
		out = append(out, citations().Resolve(
			"rp-vat-rates", packVersionOf(p), "vat.rate.standard",
			packSourceCitation(p)))
	}
	if cats := basketCats["zero_rated"]; len(cats) > 0 {
		ruleIDs := map[string]bool{}
		for cat := range cats {
			if id, ok := zeroRatedCoverageRule[strings.ToLower(cat)]; ok {
				ruleIDs[id] = true
			}
		}
		if len(ruleIDs) == 0 {
			ruleIDs["vat.rate.zero"] = true // generic zero-rate row
		}
		ids := make([]string, 0, len(ruleIDs))
		for id := range ruleIDs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		pz := s.packs.Get("rp-vat-zerorated-basket")
		pr := s.packs.Get("rp-vat-rates")
		for _, id := range ids {
			packID, pack := "rp-vat-zerorated-basket", pz
			if id == "vat.rate.zero" {
				packID, pack = "rp-vat-rates", pr
			}
			out = append(out, citations().Resolve(
				packID, packVersionOf(pack), id,
				packSourceCitation(pack)))
		}
	}
	if len(basketCats["exempt"]) > 0 {
		p := s.packs.Get("rp-vat-exempt-basket")
		out = append(out, citations().Resolve(
			"rp-vat-exempt-basket", packVersionOf(p), "vat.exempt.basket",
			packSourceCitation(p)))
	}
	return out
}

func packVersionOf(p *Pack) string {
	if p == nil {
		return ""
	}
	return p.Version
}
