package rulepack

import "testing"

func TestCitationResolverWHTDirectorsFees(t *testing.T) {
	r := NewCitationResolver("")
	c := r.Resolve("rp-wht-2024", "1.0.0", "wht.rate.directors-fees.individual",
		"WHT Regs 2024, First Schedule (KPMG rate table; UUBO; Aluko & Oyebode)")
	if c.Statute != "wht-regs-2024" {
		t.Fatalf("statute = %q, want wht-regs-2024", c.Statute)
	}
	if c.SectionID != "first-schedule.directors-fees" {
		t.Fatalf("section_id = %q", c.SectionID)
	}
	if c.Citation == "" || c.CitationKind != "secondary" {
		t.Fatalf("citation=%q kind=%q", c.Citation, c.CitationKind)
	}
	if len(c.StatuteSections) != 1 ||
		c.StatuteSections[0] != "wht-regs-2024:first-schedule.directors-fees" {
		t.Fatalf("statute_sections = %v", c.StatuteSections)
	}
	if !c.SubjectToRegazette {
		t.Fatal("wht-regs-2024 is subject_to_regazette")
	}
}

func TestCitationResolverVATStandardRate(t *testing.T) {
	r := NewCitationResolver("")
	c := r.Resolve("rp-vat-rates", "1.0.0", "vat.rate.standard", "")
	if c.Statute != "vat-act-legacy" || c.SectionID != "rate.7.5pct-from-2020-02-01" {
		t.Fatalf("got %s:%s", c.Statute, c.SectionID)
	}
	if c.CitationKind != "primary" {
		t.Fatalf("kind = %q, want primary", c.CitationKind)
	}
	if c.Citation == "" {
		t.Fatal("fallback citation from statute/section title must be non-empty")
	}
}

func TestCitationResolverNTAZeroRated(t *testing.T) {
	r := NewCitationResolver("")
	c := r.Resolve("rp-vat-zerorated-basket", "1.0.0", "vat.zero.medical-services", "")
	if c.Statute != "nta-2025" || c.SectionID != "s.187.zero-rated-medical" {
		t.Fatalf("got %s:%s", c.Statute, c.SectionID)
	}
}

func TestCitationResolverUnknownRuleDegrades(t *testing.T) {
	r := NewCitationResolver("")
	c := r.Resolve("rp-vat-rates", "1.0.0", "vat.rate.exempt", "")
	if c.Statute != "" || len(c.StatuteSections) != 0 {
		t.Fatalf("unknown rule must degrade to empty statute fields, got %+v", c)
	}
	if c.RuleID != "vat.rate.exempt" || c.PackID != "rp-vat-rates" {
		t.Fatalf("identity fields lost: %+v", c)
	}
}

func TestCitationResolverMissingDirDegrades(t *testing.T) {
	r := NewCitationResolver("/nonexistent/coverage")
	c := r.Resolve("rp-wht-2024", "1.0.0", "wht.rate.rent.company", "")
	if len(c.StatuteSections) != 0 {
		t.Fatal("missing coverage dir must yield empty index, not error")
	}
}

func TestCitationResolverBuild(t *testing.T) {
	r := NewCitationResolver("")
	cs := r.Build("rp-wht-2024", "1.0.0",
		[]string{"wht.rate.rent.company", "wht.no-tin.double-rate"},
		map[string]string{"wht.rate.rent.company": "WHT Regs 2024, First Schedule"})
	if len(cs) != 2 {
		t.Fatalf("got %d citations", len(cs))
	}
	if cs[0].SectionID != "first-schedule.rent" {
		t.Fatalf("rent section = %q", cs[0].SectionID)
	}
	if cs[1].SectionID != "reg-8.no-tin-double-rate" {
		t.Fatalf("no-tin section = %q", cs[1].SectionID)
	}
	if cs[0].Citation != "WHT Regs 2024, First Schedule" {
		t.Fatalf("pack citation not carried: %q", cs[0].Citation)
	}
}
