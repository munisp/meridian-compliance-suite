package rulepack

import "testing"

func TestLoadEmbeddedAndEvaluateWHT(t *testing.T) {
	p, err := LoadEmbedded("rp-wht-2024", "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.Ref() != "rp-wht-2024@1.0.0" {
		t.Fatalf("ref=%s", p.Ref())
	}
	// services to a company, valid TIN, N1m (<= N2m carve-out)
	d := Evaluate(p, map[string]any{
		"payment_type":         "services",
		"beneficiary":          "company",
		"has_tin":              true,
		"monthly_amount_kobo":  100000000,
		"payment_date__exists": true,
		"payment_date":         "2026-01-10",
	})
	if d.Attrs["rate_bps"] != 0 {
		t.Fatalf("expected carve-out rate 0, got %v", d.Attrs["rate_bps"])
	}
	if d.Attrs["small_company_carveout"] != true {
		t.Fatalf("expected carve-out flag, trace=%v", d.Trace)
	}
	// services, company, valid TIN, N5m -> 2%
	d2 := Evaluate(p, map[string]any{
		"payment_type":        "services",
		"beneficiary":         "company",
		"has_tin":             true,
		"monthly_amount_kobo": 500000000,
	})
	if d2.Attrs["rate_bps"] != 200 {
		t.Fatalf("expected 200bps, got %v", d2.Attrs["rate_bps"])
	}
}

func TestNoTINDoubleRate(t *testing.T) {
	p, _ := LoadEmbedded("rp-wht-2024", "")
	d := Evaluate(p, map[string]any{
		"payment_type":        "services",
		"beneficiary":         "company",
		"has_tin":             false,
		"has_nin":             false,
		"identity_ok":         false,
		"monthly_amount_kobo": 500000000,
	})
	if d.Attrs["rate_multiplier"] != 2 {
		t.Fatalf("expected double rate, got %v", d.Attrs["rate_multiplier"])
	}
	if d.Attrs["rate_bps"] != 200 {
		t.Fatalf("base rate 200 expected, got %v", d.Attrs["rate_bps"])
	}
}

func TestNINAcceptable(t *testing.T) {
	p, _ := LoadEmbedded("rp-wht-2024", "")
	d := Evaluate(p, map[string]any{
		"payment_type": "services", "beneficiary": "individual",
		"has_tin": false, "has_nin": true, "identity_ok": false,
		"monthly_amount_kobo": 500000000,
	})
	if d.Attrs["identity_ok"] != true {
		t.Fatalf("NIN should satisfy identity: %v", d.Attrs)
	}
	if _, doubled := d.Attrs["rate_multiplier"]; doubled {
		t.Fatalf("must not double when NIN present: %v", d.Attrs)
	}
}

func TestUBLPackValidationRules(t *testing.T) {
	p, err := LoadEmbedded("rp-ubl-bis", "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	d := Evaluate(p, map[string]any{
		"invoice_number": "INV-1", "issue_date": "2026-01-01", "currency": "NGN",
		"supplier_tin": "1234567890123", "supplier_name": "Acme", "customer_name": "Buyer",
		"payable_amount_kobo": 1000, "line_count": 1, "vat_category": "S",
	})
	for _, tr := range d.Trace {
		if tr.Matched {
			if v, ok := d.Attrs["violation"]; ok {
				t.Fatalf("unexpected violation %v (rule %s)", v, tr.RuleID)
			}
		}
	}
}
