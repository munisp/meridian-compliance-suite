package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/munisp/meridian-compliance-suite/packages/shared/rulepack"
)

// Validator evaluates invoices against rp-ubl-bis and rp-mbs-business-rules
// via the core rules-engine (RULES_ENGINE_URL) with the embedded-pack
// fallback (SPEC §3 T1/T2).
type Validator struct {
	eval *rulepack.Evaluator

	// now is the wall clock (injectable for tests).
	now func() time.Time
	// tinStatus resolves a TIN's registry status (active/suspended/
	// deregistered/...). Nil disables the liveness check (dev default when
	// TIN_GRAPH_URL is unset); the live default queries the TIN registry.
	tinStatus func(tin string) (string, error)
}

func NewValidator() *Validator {
	v := &Validator{eval: &rulepack.Evaluator{
		Engine:   &rulepack.EngineClient{BaseURL: os.Getenv("RULES_ENGINE_URL")},
		PacksDir: os.Getenv("RP_PACKS_DIR"),
	}}
	v.now = time.Now
	if base := strings.TrimRight(os.Getenv("TIN_GRAPH_URL"), "/"); base != "" {
		v.tinStatus = httpTINStatus(base)
	}
	return v
}

// httpTINStatus queries the TIN registry (tin-graph) for a TIN's lifecycle
// status. A configured-but-unreachable registry is a hard error (fail-closed:
// a dead TIN must never slip through because the lookup was skipped).
func httpTINStatus(base string) func(string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	return func(tin string) (string, error) {
		resp, err := client.Get(base + "/v1/tins/" + tin)
		if err != nil {
			return "", fmt.Errorf("tin registry lookup: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return "unknown", nil
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("tin registry lookup: status %d", resp.StatusCode)
		}
		var out struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", fmt.Errorf("tin registry decode: %w", err)
		}
		return out.Status, nil
	}
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
	// Pack-data-driven enforcement of the rules the engine cannot evaluate
	// (audit R4 S1a#2): mbs.date.window (backdating) and mbs.tin.live
	// (TIN liveness) carry parameters in the pack; the values below are read
	// from the pack so governance changes land via pack releases, not code.
	ev, efatal, err := v.enforceMBSRules(inv)
	if err != nil {
		return violations, fatal, err
	}
	violations = append(violations, ev...)
	return violations, fatal || efatal, nil
}

// packRule locates a rule by id in a loaded pack.
func packRule(p *rulepack.Pack, id string) *rulepack.Rule {
	for i := range p.Rules {
		if p.Rules[i].ID == id {
			return &p.Rules[i]
		}
	}
	return nil
}

// thenInt reads an integer parameter from a rule's then-map (YAML numbers
// decode as int or float64 depending on the loader).
func thenInt(r *rulepack.Rule, key string) (int, bool) {
	v, ok := r.Then[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// enforceMBSRules executes the pack-driven checks that need a wall clock or
// an external registry (the engine's when-clauses cannot express either).
// A missing rule means the pack chose not to enforce that check — absence
// of the rule disables the check, its presence enforces it.
func (v *Validator) enforceMBSRules(inv *CanonicalInvoice) (violations []Violation, fatal bool, err error) {
	p, err := v.eval.LoadPack("rp-mbs-business-rules", "")
	if err != nil {
		return nil, false, fmt.Errorf("load rp-mbs-business-rules: %w", err)
	}
	now := time.Now
	if v.now != nil {
		now = v.now
	}
	// mbs.date.window: reject invoices backdated beyond max_backdate_days.
	if r := packRule(p, "mbs.date.window"); r != nil {
		if days, ok := thenInt(r, "max_backdate_days"); ok && inv.IssueDate != "" {
			issue, perr := time.Parse("2006-01-02", inv.IssueDate)
			if perr == nil {
				// Compare on whole days so a same-calendar-day invoice is
				// never "backdated" by a few hours of clock.
				ageDays := int(now().Truncate(24*time.Hour).Sub(issue.Truncate(24*time.Hour)).Hours()/24 + 0.5)
				if ageDays > days {
					violations = append(violations, Violation{
						Pack:     p.Ref(),
						Message:  fmt.Sprintf("MBS-BR-05: invoice issue_date %s is backdated %d days (max %d per mbs.date.window)", inv.IssueDate, ageDays, days),
						Severity: "fatal",
					})
					fatal = true
				}
			}
		}
	}
	// mbs.tin.live: the supplier TIN must be live in the registry. The check
	// runs only when a registry is configured (TIN_GRAPH_URL); deregistered,
	// suspended, merged or unknown TINs are rejected outright.
	if r := packRule(p, "mbs.tin.live"); r != nil && v.tinStatus != nil {
		tin := strings.TrimSpace(inv.Supplier.TIN)
		if tin != "" {
			status, lerr := v.tinStatus(tin)
			if lerr != nil {
				return violations, fatal, fmt.Errorf("mbs.tin.live: %w", lerr)
			}
			if status != "active" {
				violations = append(violations, Violation{
					Pack:     p.Ref(),
					Message:  fmt.Sprintf("MBS-BR-06: supplier TIN %s is not live (registry status %q) per mbs.tin.live", tin, status),
					Severity: "fatal",
				})
				fatal = true
			}
		}
	}
	return violations, fatal, nil
}
