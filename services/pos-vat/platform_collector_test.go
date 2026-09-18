package main

// platform_collector_test.go — regression (R4 S1a#8): packs.go's
// IsPlatformCollector was dead code and the designated platform-collectors
// regime (rp-platform-collectors) never ran. It is now wired into the
// ingest path (receipts sold through a designated platform are flagged
// platform_collected with the platform as collector of record) and into
// settlement recon (the platform-collected VAT leg is surfaced and
// remitted).

import (
	"testing"
)

func newPlatformService(t *testing.T) *Service {
	t.Helper()
	s := newSettleService(t)
	s.packs.LoadPacks() // as main() does at startup
	return s
}

func ingestPlatformReceipt(t *testing.T, s *Service, rc *Receipt) *Receipt {
	t.Helper()
	out, err := s.processReceipt(rc, rc.MerchantTIN+":"+rc.ReceiptNo)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.PutReceipt(out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPlatformCollectorReceiptCollectsMarketplaceVAT(t *testing.T) {
	s := newPlatformService(t)

	// Marketplace sale through a designated platform collector (pack prefix
	// PLTC/MRKT/ECOM): the platform collects the VAT.
	rc := &Receipt{
		TenantID: "tenant-f5", MerchantTIN: "PLTC-JUMIA-001",
		TerminalID: "t1", ReceiptNo: "r1",
		CapturedAt: "2026-01-15T10:00:00Z", State: "Lagos", Lat: 6.45, Lon: 3.39,
		Lines: []ReceiptLine{{SKU: "s1", Qty: 1000, UnitPrice: 100_000, Category: "general"}},
	}
	out := ingestPlatformReceipt(t, s, rc)
	if out.VATKobo != 7_500 { // 7.5% of N1,000.00
		t.Fatalf("vat=%d", out.VATKobo)
	}
	if !out.PlatformCollected || out.CollectorTIN != "PLTC-JUMIA-001" {
		t.Fatalf("platform flags: %+v", out)
	}

	// Ordinary merchant: not platform-collected.
	rc2 := &Receipt{
		TenantID: "tenant-f5", MerchantTIN: "1234567890123",
		TerminalID: "t1", ReceiptNo: "r2",
		CapturedAt: "2026-01-15T11:00:00Z", State: "Lagos", Lat: 6.45, Lon: 3.39,
		Lines: []ReceiptLine{{SKU: "s1", Qty: 1000, UnitPrice: 100_000, Category: "general"}},
	}
	out2 := ingestPlatformReceipt(t, s, rc2)
	if out2.PlatformCollected {
		t.Fatalf("ordinary merchant wrongly platform-collected: %+v", out2)
	}

	// Settlement recon surfaces the platform-collected leg and the VAT is
	// actually remitted (pools credited).
	code, resp := settleReq(t, s, "2026-01")
	if code != 201 {
		t.Fatalf("settle: code=%d out=%v", code, resp)
	}
	if got := resp["platform_collected_kobo"]; got != float64(7_500) {
		t.Fatalf("platform_collected_kobo=%v", resp)
	}
	if got := resp["platform_receipts"]; got != float64(1) {
		t.Fatalf("platform_receipts=%v", resp)
	}
	if resp["vat_kobo"] != float64(15_000) {
		t.Fatalf("total vat=%v", resp)
	}
	// Platform VAT was remitted into the shared pools (federal+state legs).
	if poolBal(t, s, NSVATFederalPool)+poolBal(t, s, NSVATStatePool) == 0 {
		t.Fatal("settlement pools empty — platform VAT not remitted")
	}
}

// Pack-driven prefixes govern the designation (tin_prefixes in the pack).
func TestPlatformCollectorPrefixesPackDriven(t *testing.T) {
	s := newPlatformService(t)
	for _, tin := range []string{"PLTC-1", "MRKT-2", "ECOM-3"} {
		if !s.packs.IsPlatformCollector(tin) {
			t.Errorf("prefix %s must be a designated platform collector", tin)
		}
	}
	if s.packs.IsPlatformCollector("1234567890123") {
		t.Error("ordinary TIN wrongly designated a platform collector")
	}
}
