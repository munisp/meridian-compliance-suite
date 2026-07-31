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
	// Canonical pack contexts (rule-packs repo vocabulary, SPEC §1.4 operator maps).
	// Small-company carve-out (WHT Regs 2024 reg. 4, tax-law-parity pack): keys
	// on the PAYER being small + month value <= N2m + supplier TIN -> rate 0.
	d := Evaluate(p, map[string]any{
		"payment_type":                 "supply_of_goods_materials",
		"beneficiary":                  "company",
		"supplier_tin":                 "12345678-0001",
		"payer_size":                   "small",
		"payer_annual_turnover_kobo":   2000000000,
		"transaction_month_value_kobo": 100000000,
		"payment_event":                "payment",
	})
	if d.Attrs["rate_bps"] != 0 {
		t.Fatalf("expected carve-out rate 0, got %v", d.Attrs["rate_bps"])
	}
	// generic other-services, company, valid TIN -> 2% resident (2024 Regs rate)
	d2 := Evaluate(p, map[string]any{
		"payment_type":                   "services",
		"beneficiary":                    "company",
		"supplier_tin":                   "12345678-0001",
		"supplier_monthly_turnover_kobo": 500000000,
	})
	if d2.Attrs["rate_bps"] != 200 {
		t.Fatalf("expected 200bps, got %v", d2.Attrs["rate_bps"])
	}
	// royalty: company 10%, individual 5% (embedded drift had them swapped)
	d3 := Evaluate(p, map[string]any{"payment_type": "royalty", "beneficiary": "company", "supplier_tin": "t"})
	if d3.Attrs["rate_bps"] != 1000 {
		t.Fatalf("royalty company expected 1000bps, got %v", d3.Attrs["rate_bps"])
	}
	d4 := Evaluate(p, map[string]any{"payment_type": "royalty", "beneficiary": "individual", "supplier_nin": "n"})
	if d4.Attrs["rate_bps"] != 500 {
		t.Fatalf("royalty individual expected 500bps, got %v", d4.Attrs["rate_bps"])
	}
}

func TestNoTINDoubleRate(t *testing.T) {
	p, _ := LoadEmbedded("rp-wht-2024", "")
	d := Evaluate(p, map[string]any{
		"payment_type": "services",
		"beneficiary":  "company",
		// no supplier_tin, no supplier_nin -> Reg 5 double rate
	})
	if d.Attrs["rate_multiplier_bps"] != 20000 {
		t.Fatalf("expected double rate multiplier 20000bps, got %v", d.Attrs["rate_multiplier_bps"])
	}
	if d.Attrs["rate_bps"] != 200 {
		t.Fatalf("base rate 200 (generic other-services resident) expected, got %v", d.Attrs["rate_bps"])
	}
}

func TestNINAcceptable(t *testing.T) {
	p, _ := LoadEmbedded("rp-wht-2024", "")
	d := Evaluate(p, map[string]any{
		"payment_type": "services", "beneficiary": "individual",
		"supplier_nin": "12345678901",
	})
	if d.Attrs["decision"] != "identity_satisfied" {
		t.Fatalf("NIN should satisfy identity: %v", d.Attrs)
	}
	if _, doubled := d.Attrs["rate_multiplier_bps"]; doubled {
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
