package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/munisp/meridian-compliance-suite/packages/shared/rulepack"
)

// Validator evaluates invoices against rp-ubl-bis and rp-mbs-business-rules
// via the core rules-engine (RULES_ENGINE_URL) with the embedded-pack
// fallback (SPEC §3 T1/T2).
type Validator struct {
	eval *rulepack.Evaluator
}

func NewValidator() *Validator {
	return &Validator{eval: &rulepack.Evaluator{
		Engine:   &rulepack.EngineClient{BaseURL: os.Getenv("RULES_ENGINE_URL")},
		PacksDir: os.Getenv("RP_PACKS_DIR"),
	}}
}

// PackVersions reports which pack refs will be used (for observability).
func (v *Validator) PackVersions() map[string]string {
	out := map[string]string{}
	for _, id := range []string{"rp-ubl-bis", "rp-mbs-business-rules"} {
		if p, err := v.eval.LoadPack(id, ""); err == nil {
			out[id] = p.Ref()
		}
	}
	return out
}

// Validate runs both packs and collects violations. Returns fatal=true when
// any fatal-severity rule fired.
func (v *Validator) Validate(inv *CanonicalInvoice, duplicate bool) (violations []Violation, fatal bool, err error) {
	ctx := inv.RuleContext(duplicate)
	for _, packID := range []string{"rp-ubl-bis", "rp-mbs-business-rules"} {
		d, err := v.eval.Evaluate(packID, "", ctx)
		if err != nil {
			return nil, false, fmt.Errorf("evaluate %s: %w", packID, err)
		}
		if msg, ok := d.Attrs["violation"].(string); ok && msg != "" {
			sev, _ := d.Attrs["severity"].(string)
			if sev == "" {
				sev = "fatal"
			}
			violations = append(violations, Violation{Pack: d.Pack, Message: msg, Severity: sev})
			if sev == "fatal" {
				fatal = true
			}
		}
		// Multiple violations: walk trace and re-collect per-rule then payload.
		for _, tr := range d.Trace {
			if !tr.Matched {
				continue
			}
			p, lerr := v.eval.LoadPack(packID, "")
			if lerr != nil {
				continue
			}
			for _, r := range p.Rules {
				if r.ID != tr.RuleID {
					continue
				}
				msg, _ := r.Then["violation"].(string)
				if msg == "" {
					continue
				}
				sev, _ := r.Then["severity"].(string)
				if sev == "" {
					sev = "fatal"
				}
				// dedupe
				seen := false
				for _, ex := range violations {
					if ex.Message == msg && strings.HasPrefix(ex.Pack, packID) {
						seen = true
						break
					}
				}
				if !seen {
					violations = append(violations, Violation{Pack: p.Ref(), Message: msg, Severity: sev})
					if sev == "fatal" {
						fatal = true
					}
				}
			}
		}
	}
	return violations, fatal, nil
}
