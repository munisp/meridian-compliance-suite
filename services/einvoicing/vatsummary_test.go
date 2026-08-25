package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/munisp/meridian-compliance-suite/packages/shared/devjwt"
)

func seedInvoice(t *testing.T, s *Server, tenant, number, issueDate, status string, excl, tax int64) {
	t.Helper()
	inv := sampleInvoice()
	inv.InvoiceNumber = number // distinct per row: supplier+number dedup
	inv.TenantID = tenant
	inv.IssueDate = issueDate
	inv.Status = status
	inv.TaxExclusiveKobo = excl
	inv.TaxKobo = tax
	inv.PayableKobo = excl + tax
	if _, err := s.store.Save(inv); err != nil {
		t.Fatal(err)
	}
}

func TestVATSummaryAggregation(t *testing.T) {
	s := newBOLAServer(t)
	// tenant-a: two Jan invoices, one Feb; mixed statuses
	seedInvoice(t, s, "tenant-a", "A-1", "2026-01-05", "validated", 1_000_000, 75_000)
	seedInvoice(t, s, "tenant-a", "A-2", "2026-01-20", "reported", 2_000_000, 150_000)
	seedInvoice(t, s, "tenant-a", "A-3", "2026-02-02", "failed", 500_000, 37_500)
	// tenant-b rows must never appear in tenant-a's summary
	seedInvoice(t, s, "tenant-b", "B-1", "2026-01-11", "validated", 9_000_000, 675_000)
	// tenant-less legacy row excluded too
	seedInvoice(t, s, "", "L-1", "2026-01-12", "validated", 7_000_000, 525_000)

	req := httptest.NewRequest("GET", "/v1/vat/summary", nil)
	req.Header.Set("X-Dev-Role", "auditor") // any authenticated role may read
	req.Header.Set("X-Tenant-ID", "tenant-a")
	rec := httptest.NewRecorder()
	devjwt.Middleware(http.HandlerFunc(s.handleVATSummary)).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("summary=%d %s", rec.Code, rec.Body)
	}
	var sum VATSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.TenantID != "tenant-a" || sum.TotalInvoices != 3 {
		t.Fatalf("totals: tenant=%s n=%d", sum.TenantID, sum.TotalInvoices)
	}
	if sum.TotalTaxKobo != 75_000+150_000+37_500 {
		t.Fatalf("total tax=%d", sum.TotalTaxKobo)
	}
	if sum.TotalPayableKobo != 1_075_000+2_150_000+537_500 {
		t.Fatalf("total payable=%d", sum.TotalPayableKobo)
	}
	if len(sum.Periods) != 2 {
		t.Fatalf("periods=%v", sum.Periods)
	}
	jan, feb := sum.Periods[0], sum.Periods[1]
	if jan.Period != "2026-01" || jan.InvoiceCount != 2 || jan.TaxKobo != 225_000 || jan.PayableKobo != 3_225_000 {
		t.Fatalf("jan bucket=%+v", jan)
	}
	if feb.Period != "2026-02" || feb.InvoiceCount != 1 || feb.TaxKobo != 37_500 {
		t.Fatalf("feb bucket=%+v", feb)
	}
	want := map[string]int{"validated": 1, "reported": 1, "failed": 1}
	for st, n := range want {
		if sum.ByStatus[st] != n {
			t.Fatalf("by_status[%s]=%d want %d (%v)", st, sum.ByStatus[st], n, sum.ByStatus)
		}
	}
	// tenant-b sees only its own figures
	req = httptest.NewRequest("GET", "/v1/vat/summary", nil)
	req.Header.Set("X-Dev-Role", "operator")
	req.Header.Set("X-Tenant-ID", "tenant-b")
	rec = httptest.NewRecorder()
	devjwt.Middleware(http.HandlerFunc(s.handleVATSummary)).ServeHTTP(rec, req)
	var sumB VATSummary
	_ = json.Unmarshal(rec.Body.Bytes(), &sumB)
	if sumB.TotalInvoices != 1 || sumB.TotalTaxKobo != 675_000 {
		t.Fatalf("tenant-b summary=%+v", sumB)
	}
}

func TestVATSummaryRequiresTenant(t *testing.T) {
	s := newBOLAServer(t)
	req := httptest.NewRequest("GET", "/v1/vat/summary", nil)
	req.Header.Set("X-Dev-Role", "admin") // no X-Tenant-ID -> empty claim
	rec := httptest.NewRecorder()
	devjwt.Middleware(http.HandlerFunc(s.handleVATSummary)).ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("tenant-less summary=%d (want 403)", rec.Code)
	}
	// unauthenticated -> 401
	req = httptest.NewRequest("GET", "/v1/vat/summary", nil)
	rec = httptest.NewRecorder()
	devjwt.Middleware(http.HandlerFunc(s.handleVATSummary)).ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("anon summary=%d (want 401)", rec.Code)
	}
}

func TestVATSummaryUndatedFallback(t *testing.T) {
	invs := []*CanonicalInvoice{{
		TenantID: "t", Status: "validated", TaxKobo: 10, PayableKobo: 110,
		CreatedAt: time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC),
	}}
	sum := SummarizeVAT(invs, "t", time.Now())
	if len(sum.Periods) != 1 || sum.Periods[0].Period != "2026-03" || sum.Periods[0].TaxKobo != 10 {
		t.Fatalf("undated fallback=%+v", sum.Periods)
	}
}
