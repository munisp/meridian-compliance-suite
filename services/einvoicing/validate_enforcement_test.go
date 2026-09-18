package main

import (
	"fmt"
	"testing"
	"time"
)

// Regression (R4 S1a#2): the pack validator only collected `violation`
// attrs and rp-mbs-business-rules/rp-ubl-bis emitted none, so
// mbs.date.window (backdating) and mbs.tin.live (dead TIN) never fired.
// Both are now enforced pack-data-driven: the pack rules govern, code
// executes them.

func newEnforceValidator(now time.Time, tinStatus func(string) (string, error)) *Validator {
	v := NewValidator()
	v.now = func() time.Time { return now }
	v.tinStatus = tinStatus
	return v
}

func hasFatal(vs []Violation) bool {
	for _, v := range vs {
		if v.Severity == "fatal" {
			return true
		}
	}
	return false
}

func TestBackdatedInvoiceRejected(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	v := newEnforceValidator(now, nil)

	inv := sampleInvoice()
	inv.IssueDate = "2026-03-09" // within the 7-day window
	vs, fatal, err := v.Validate(inv, false)
	if err != nil {
		t.Fatal(err)
	}
	if fatal {
		t.Fatalf("in-window invoice must not be fatal: %+v", vs)
	}

	inv.IssueDate = "2026-02-01" // 37 days back — beyond max_backdate_days=7
	vs, fatal, err = v.Validate(inv, false)
	if err != nil {
		t.Fatal(err)
	}
	if !fatal || !hasFatal(vs) {
		t.Fatalf("backdated invoice must be rejected: fatal=%v vs=%+v", fatal, vs)
	}
	found := false
	for _, vi := range vs {
		if fmt.Sprint(vi.Message) != "" && vi.Pack != "" && vi.Severity == "fatal" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a fatal pack-tagged violation, got %+v", vs)
	}
}

func TestDeregisteredTINRejected(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	registry := map[string]string{
		"1234567890123": "active",
		"5555555555555": "deregistered",
		"6666666666666": "suspended",
	}
	v := newEnforceValidator(now, func(tin string) (string, error) {
		if s, ok := registry[tin]; ok {
			return s, nil
		}
		return "unknown", nil
	})

	inv := sampleInvoice()
	inv.IssueDate = "2026-03-10"
	inv.Supplier.TIN = "1234567890123"
	if _, fatal, err := v.Validate(inv, false); err != nil || fatal {
		t.Fatalf("active TIN must pass: fatal=%v err=%v", fatal, err)
	}

	for _, tin := range []string{"5555555555555", "6666666666666", "9999999999999"} {
		inv.Supplier.TIN = tin
		vs, fatal, err := v.Validate(inv, false)
		if err != nil {
			t.Fatal(err)
		}
		if !fatal || !hasFatal(vs) {
			t.Fatalf("TIN %s (non-live) must be rejected: %+v", tin, vs)
		}
	}
}

// A configured-but-failing registry is fail-closed (error, not silent pass).
func TestTINRegistryErrorFailsClosed(t *testing.T) {
	v := newEnforceValidator(time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC),
		func(string) (string, error) { return "", fmt.Errorf("connection refused") })
	inv := sampleInvoice()
	inv.IssueDate = "2026-03-10"
	if _, _, err := v.Validate(inv, false); err == nil {
		t.Fatal("unreachable TIN registry must fail closed, not silently pass")
	}
}

// The pack values govern: removing the rule from the pack would disable the
// check — here we assert the pack actually carries the parameters.
func TestPackCarriesEnforcementRules(t *testing.T) {
	v := NewValidator()
	p, err := v.eval.LoadPack("rp-mbs-business-rules", "")
	if err != nil {
		t.Fatal(err)
	}
	r := packRule(p, "mbs.date.window")
	if r == nil {
		t.Fatal("pack missing mbs.date.window")
	}
	days, ok := thenInt(r, "max_backdate_days")
	if !ok || days != 7 {
		t.Fatalf("max_backdate_days = %d,%v want 7", days, ok)
	}
	if packRule(p, "mbs.tin.live") == nil {
		t.Fatal("pack missing mbs.tin.live")
	}
}
