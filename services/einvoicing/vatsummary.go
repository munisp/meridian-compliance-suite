// vatsummary.go — VAT dashboard aggregation (feature I5). GET /v1/vat/summary
// computes real totals from the canonical invoice store for the caller's
// tenant: per-period (YYYY-MM) VAT/exclusive/payable totals and counts by
// invoice status. No fixture data — every figure derives from stored rows.
package main

import (
	"net/http"
	"sort"
	"time"

	"github.com/munisp/meridian-compliance-suite/packages/shared/devjwt"
)

// VATPeriod is one calendar month of aggregated invoice VAT.
type VATPeriod struct {
	Period           string `json:"period"` // YYYY-MM (from issue_date, fallback created_at)
	InvoiceCount     int    `json:"invoice_count"`
	TaxExclusiveKobo int64  `json:"tax_exclusive_kobo"`
	TaxKobo          int64  `json:"tax_kobo"`
	PayableKobo      int64  `json:"payable_kobo"`
}

// VATSummary is the dashboard payload for one tenant.
type VATSummary struct {
	TenantID         string         `json:"tenant_id"`
	GeneratedAt      time.Time      `json:"generated_at"`
	Periods          []VATPeriod    `json:"periods"`   // ascending by period
	ByStatus         map[string]int `json:"by_status"` // status -> invoice count
	TotalInvoices    int            `json:"total_invoices"`
	TotalTaxKobo     int64          `json:"total_tax_kobo"`
	TotalPayableKobo int64          `json:"total_payable_kobo"`
}

// invoicePeriod buckets an invoice by its issue month; undated rows fall
// back to the creation month so no row silently drops out of the totals.
func invoicePeriod(inv *CanonicalInvoice) string {
	if len(inv.IssueDate) >= 7 {
		return inv.IssueDate[:7]
	}
	if !inv.CreatedAt.IsZero() {
		return inv.CreatedAt.UTC().Format("2006-01")
	}
	return "undated"
}

// SummarizeVAT aggregates the tenant's invoices. Money stays integer kobo.
func SummarizeVAT(invs []*CanonicalInvoice, tenantID string, now time.Time) VATSummary {
	sum := VATSummary{
		TenantID: tenantID, GeneratedAt: now.UTC(),
		Periods: []VATPeriod{}, ByStatus: map[string]int{},
	}
	byPeriod := map[string]*VATPeriod{}
	for _, inv := range invs {
		// caller-tenant rows only; tenant-less legacy rows are excluded
		// (the dashboard must never leak cross-tenant figures).
		if inv.TenantID == "" || inv.TenantID != tenantID {
			continue
		}
		p := invoicePeriod(inv)
		bucket, ok := byPeriod[p]
		if !ok {
			bucket = &VATPeriod{Period: p}
			byPeriod[p] = bucket
		}
		bucket.InvoiceCount++
		bucket.TaxExclusiveKobo += inv.TaxExclusiveKobo
		bucket.TaxKobo += inv.TaxKobo
		bucket.PayableKobo += inv.PayableKobo
		status := inv.Status
		if status == "" {
			status = "unknown"
		}
		sum.ByStatus[status]++
		sum.TotalInvoices++
		sum.TotalTaxKobo += inv.TaxKobo
		sum.TotalPayableKobo += inv.PayableKobo
	}
	for _, b := range byPeriod {
		sum.Periods = append(sum.Periods, *b)
	}
	sort.Slice(sum.Periods, func(i, j int) bool { return sum.Periods[i].Period < sum.Periods[j].Period })
	return sum
}

// handleVATSummary serves the merchant VAT dashboard. Any authenticated role
// may read it, but the tenant claim is mandatory — an unscoped principal
// must never see an aggregate spanning tenants.
func (s *Server) handleVATSummary(w http.ResponseWriter, r *http.Request) {
	claims, ok := devjwt.FromContext(r)
	if !ok || claims.Sub == "" {
		devjwt.Problem(w, 401, "unauthorized", "authentication required")
		return
	}
	if claims.TenantID == "" {
		devjwt.Problem(w, 403, "forbidden", "tenant claim required")
		return
	}
	writeJSON(w, 200, SummarizeVAT(s.store.List(), claims.TenantID, time.Now()))
}
