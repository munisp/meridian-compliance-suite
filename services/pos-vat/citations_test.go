package main

// LCE SPEC §5 — pos-vat receipt responses carry statute citations per
// computed VAT amount. Additive field; existing behavior untouched.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	s := NewService(Config{DataDir: t.TempDir(), AuthMode: "dev"})
	s.packs.LoadPacks() // as main() does at startup
	return s
}

func ingestReceipt(t *testing.T, s *Service, lines []map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{
		"merchant_tin": "1234567890123", "terminal_id": "T1",
		"receipt_no": "R-CIT-1", "lat": 6.5244, "lon": 3.3792,
		"lines": lines,
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/receipts", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	s.handleIngestReceipt(rec, req)
	if rec.Code != 201 {
		t.Fatalf("ingest status %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp["receipt"].(map[string]any)
}

func citationFor(rc map[string]any, ruleID string) map[string]any {
	for _, c := range rc["citations"].([]any) {
		m := c.(map[string]any)
		if m["rule_id"] == ruleID {
			return m
		}
	}
	return nil
}

func TestReceiptCitationsStandardRate(t *testing.T) {
	s := newTestService(t)
	rc := ingestReceipt(t, s, []map[string]any{
		{"sku": "A1", "qty_milli": 1000, "unit_price_kobo": 100000, "category": "electronics"},
	})
	cits, ok := rc["citations"].([]any)
	if !ok || len(cits) == 0 {
		t.Fatalf("expected citations on receipt, got %v", rc["citations"])
	}
	c := citationFor(rc, "vat.rate.standard")
	if c == nil {
		t.Fatalf("no vat.rate.standard citation in %v", cits)
	}
	if c["statute"] != "vat-act-legacy" ||
		c["section_id"] != "rate.7.5pct-from-2020-02-01" {
		t.Fatalf("standard rate citation = %v", c)
	}
	if c["citation_kind"] != "primary" {
		t.Fatalf("VAT Act row is primary, got %v", c["citation_kind"])
	}
	// integer kobo untouched
	if rc["vat_kobo"].(float64) != 7500 {
		t.Fatalf("vat_kobo = %v", rc["vat_kobo"])
	}
}

func TestReceiptCitationsZeroRatedNTA187(t *testing.T) {
	s := newTestService(t)
	rc := ingestReceipt(t, s, []map[string]any{
		{"sku": "M1", "qty_milli": 1000, "unit_price_kobo": 50000, "category": "medical"},
		{"sku": "E1", "qty_milli": 1000, "unit_price_kobo": 30000, "category": "tuition"},
	})
	if rc["vat_kobo"].(float64) != 0 {
		t.Fatalf("zero-rated vat_kobo = %v", rc["vat_kobo"])
	}
	med := citationFor(rc, "vat.zero.medical-services")
	if med == nil || med["statute"] != "nta-2025" ||
		med["section_id"] != "s.187.zero-rated-medical" {
		t.Fatalf("medical zero-rated citation = %v", med)
	}
	tu := citationFor(rc, "vat.zero.education-tuition")
	if tu == nil || tu["section_id"] != "s.187.zero-rated-tuition" {
		t.Fatalf("tuition zero-rated citation = %v", tu)
	}
	// NTA rows are secondary until CTC verification (registry workstream)
	if med["citation_kind"] != "secondary" {
		t.Fatalf("citation_kind = %v", med["citation_kind"])
	}
}

func TestReceiptCitationsMixedBasketAndExemptFallback(t *testing.T) {
	s := newTestService(t)
	rc := ingestReceipt(t, s, []map[string]any{
		{"sku": "A1", "qty_milli": 1000, "unit_price_kobo": 100000, "category": "electronics"},
		{"sku": "F1", "qty_milli": 1000, "unit_price_kobo": 20000, "category": "land_sale"},
	})
	if citationFor(rc, "vat.rate.standard") == nil {
		t.Fatal("missing standard rate citation")
	}
	ex := citationFor(rc, "vat.exempt.basket")
	if ex == nil {
		t.Fatal("missing exempt basket citation")
	}
	// no coverage hit -> statute fields empty, citation falls back to pack
	// provenance (NTA 2025 First Schedule) — degraded, never an error
	if ex["citation"] == "" {
		t.Fatal("exempt fallback citation empty")
	}
}
