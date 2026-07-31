package main

// LCE SPEC §5 — einvoicing responses carry per-line VAT statute citations.
// Additive field; existing invoice behavior untouched.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func createInvoiceFull(t *testing.T, srv *Server, inv *CanonicalInvoice) *CanonicalInvoice {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/invoices", srv.handleCreateInvoice)
	inv.ID = ""
	body, _ := json.Marshal(inv)
	req := httptest.NewRequest("POST", "/v1/invoices", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		Invoices []*CanonicalInvoice `json:"invoices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return created.Invoices[0]
}

func TestLineCitationsStandardRated(t *testing.T) {
	srv := newTestServer(t)
	inv := sampleInvoice()
	got := createInvoiceFull(t, srv, inv)
	if len(got.Lines) == 0 {
		t.Fatal("no lines")
	}
	for _, l := range got.Lines {
		if len(l.Citations) == 0 {
			t.Fatalf("line %s has no citations", l.ID)
		}
		c := l.Citations[0]
		if l.VatCategory == "S" {
			if c.RuleID != "vat.rate.standard" ||
				c.Statute != "vat-act-legacy" ||
				c.SectionID != "rate.7.5pct-from-2020-02-01" {
				t.Fatalf("standard line citation = %+v", c)
			}
			if c.CitationKind != "primary" {
				t.Fatalf("VAT Act row is primary, got %q", c.CitationKind)
			}
			if len(c.StatuteSections) != 1 {
				t.Fatalf("statute_sections = %v", c.StatuteSections)
			}
		}
		// integer kobo untouched
		if l.VatAmountKobo != RoundBpsHalfUp(l.LineTotalKobo, l.VatRateBps) {
			t.Fatalf("vat_amount_kobo changed: %+v", l)
		}
	}
}

func TestLineCitationsZeroAndExempt(t *testing.T) {
	srv := newTestServer(t)
	inv := sampleInvoice()
	inv.Lines = []InvoiceLine{
		{Description: "hospital service", QuantityMilli: 1000,
			UnitPriceKobo: 50000, VatCategory: "Z", VatRateBps: 0},
		{Description: "land sale", QuantityMilli: 1000,
			UnitPriceKobo: 90000, VatCategory: "E", VatRateBps: 0},
	}
	got := createInvoiceFull(t, srv, inv)
	z := got.Lines[0].Citations[0]
	if z.RuleID != "vat.rate.zero" || z.Citation == "" {
		t.Fatalf("zero-rated citation = %+v", z)
	}
	// rp-vat-rates:vat.rate.zero has no coverage row -> statute fields empty,
	// fallback citation text carried (degraded, never an error)
	if len(z.StatuteSections) != 0 {
		t.Fatalf("unexpected statute sections: %v", z.StatuteSections)
	}
	e := got.Lines[1].Citations[0]
	if e.RuleID != "vat.rate.exempt" || e.Citation == "" {
		t.Fatalf("exempt citation = %+v", e)
	}
	if got.Lines[0].VatAmountKobo != 0 || got.Lines[1].VatAmountKobo != 0 {
		t.Fatalf("zero/exempt VAT changed: %+v", got.Lines)
	}
}

func TestCitationOmittedWhenEmpty(t *testing.T) {
	// lines with an unknown category carry no citations key at all (omitempty)
	inv := &CanonicalInvoice{Lines: []InvoiceLine{{
		Description: "x", QuantityMilli: 1000, UnitPriceKobo: 100,
		VatCategory: "unknown-cat"}}}
	inv.Normalise()
	if inv.Lines[0].Citations != nil {
		t.Fatalf("expected nil citations, got %+v", inv.Lines[0].Citations)
	}
	b, _ := json.Marshal(inv.Lines[0])
	if bytes.Contains(b, []byte("citations")) {
		t.Fatalf("citations key must be omitted: %s", b)
	}
}
