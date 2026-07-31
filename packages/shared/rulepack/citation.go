// Citation resolver — LCE SPEC §5 runtime citation chain.
//
// Every computed tax amount carries its statute citations so responses are
// auditable by construction. The resolver builds a reverse index from the
// canonical coverage matrix (coverage/*.yaml, vendored copy embedded below;
// override with env LCE_COVERAGE_DIR) mapping
//
//	"<pack_id>:<rule_id>" -> statute section
//
// and merges it with the short citation text carried on the pack rule itself.
// Resolution is read-only and total: a missing coverage file or unknown rule
// yields empty statute fields, never an error (SPEC §5.1 rule b).
//
// Citation schema (SPEC §5.1):
//
//	{
//	  "pack_id": "rp-wht-2024",
//	  "pack_version": "1.0.0",
//	  "rule_id": "wht.rate.directors-fees.non-resident",
//	  "statute": "wht-regs-2024",
//	  "section_id": "first-schedule.directors-fees",
//	  "title": "Directors' fees — 15% resident individual / 20% non-resident (final)",
//	  "citation": "WHT Regs 2024, First Schedule (KPMG rate table — ...)",
//	  "statute_sections": ["wht-regs-2024:first-schedule.directors-fees"],
//	  "citation_kind": "secondary",
//	  "subject_to_regazette": true
//	}
//
// [REAL] resolution from the coverage matrix; matrix content citation_kind is
// "secondary" until CTC verification (registry workstream owns the CTC feed;
// unknown per-row fields such as ctc:* are tolerated).
package rulepack

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed coverage
var embeddedCoverage embed.FS

// Citation is the per-rule statute citation attached to computed amounts.
type Citation struct {
	PackID             string   `json:"pack_id"`
	PackVersion        string   `json:"pack_version"`
	RuleID             string   `json:"rule_id"`
	Statute            string   `json:"statute,omitempty"`
	SectionID          string   `json:"section_id,omitempty"`
	Title              string   `json:"title,omitempty"`
	Citation           string   `json:"citation"`
	StatuteSections    []string `json:"statute_sections"`
	CitationKind       string   `json:"citation_kind,omitempty"`
	SubjectToRegazette bool     `json:"subject_to_regazette"`
}

type coverageStatute struct {
	ID                 string `yaml:"id"`
	Title              string `yaml:"title"`
	SubjectToRegazette bool   `yaml:"subject_to_regazette"`
}

type coverageSection struct {
	SectionID         string   `yaml:"section_id"`
	Title             string   `yaml:"title"`
	ImplementingRules []string `yaml:"implementing_rules"`
	CitationKind      string   `yaml:"citation_kind"`
}

type coverageDoc struct {
	Statute  coverageStatute   `yaml:"statute"`
	Sections []coverageSection `yaml:"sections"`
}

type sectionHit struct {
	statute coverageStatute
	section coverageSection
}

// CitationResolver resolves matched pack rules to statute citations.
type CitationResolver struct {
	index map[string][]sectionHit // "<pack>:<rule>" -> hits
}

// NewCitationResolver builds the reverse index from the coverage matrix in
// dir. dir "" means: $LCE_COVERAGE_DIR if set, else the vendored embedded
// copy. Unreadable/missing files are skipped (never an error).
func NewCitationResolver(dir string) *CitationResolver {
	r := &CitationResolver{index: map[string][]sectionHit{}}
	if dir == "" {
		dir = os.Getenv("LCE_COVERAGE_DIR")
	}
	if dir != "" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return r // degrade to empty index
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			r.addDoc(data)
		}
		return r
	}
	_ = fs.WalkDir(embeddedCoverage, "coverage", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		data, err := embeddedCoverage.ReadFile(path)
		if err != nil {
			return nil
		}
		r.addDoc(data)
		return nil
	})
	return r
}

func (r *CitationResolver) addDoc(data []byte) {
	var doc coverageDoc
	if err := yaml.Unmarshal(data, &doc); err != nil || doc.Statute.ID == "" {
		return
	}
	for _, sec := range doc.Sections {
		for _, ref := range sec.ImplementingRules {
			key := strings.TrimSpace(ref)
			if key == "" {
				continue
			}
			r.index[key] = append(r.index[key], sectionHit{doc.Statute, sec})
		}
	}
}

// Resolve returns the citation for one matched rule. ruleCitation is the
// short citation text from the pack rule ("" when the rule carries none).
// Unknown rules resolve to a citation with empty statute fields.
func (r *CitationResolver) Resolve(packID, packVersion, ruleID, ruleCitation string) Citation {
	c := Citation{
		PackID:          packID,
		PackVersion:     packVersion,
		RuleID:          ruleID,
		Citation:        ruleCitation,
		StatuteSections: []string{},
	}
	hits := r.index[fmt.Sprintf("%s:%s", packID, ruleID)]
	if len(hits) == 0 {
		return c
	}
	// Deterministic: sort hits by statute:section.
	sort.Slice(hits, func(i, j int) bool {
		a := hits[i].statute.ID + ":" + hits[i].section.SectionID
		b := hits[j].statute.ID + ":" + hits[j].section.SectionID
		return a < b
	})
	first := hits[0]
	c.Statute = first.statute.ID
	c.SectionID = first.section.SectionID
	c.Title = first.section.Title
	c.CitationKind = first.section.CitationKind
	c.SubjectToRegazette = first.statute.SubjectToRegazette
	if c.Citation == "" {
		c.Citation = strings.TrimSpace(first.statute.Title + ", " + first.section.Title)
	}
	for _, h := range hits {
		c.StatuteSections = append(c.StatuteSections,
			h.statute.ID+":"+h.section.SectionID)
	}
	return c
}

// Build resolves one citation per matched rule id. ruleCitations maps rule id
// to its pack-level citation text (may be nil/empty).
func (r *CitationResolver) Build(packID, packVersion string, ruleIDs []string, ruleCitations map[string]string) []Citation {
	out := make([]Citation, 0, len(ruleIDs))
	for _, id := range ruleIDs {
		out = append(out, r.Resolve(packID, packVersion, id, ruleCitations[id]))
	}
	return out
}
