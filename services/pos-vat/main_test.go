package main

import (
	"strings"
	"testing"
)

func TestYAMLParsePack(t *testing.T) {
	p, err := packFromYAML("rp-vat-rates", embeddedPacks["rp-vat-rates"], "embedded")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.ID != "rp-vat-rates" || p.Version != "1.0.0" {
		t.Fatalf("bad pack meta: %+v", p)
	}
	if len(p.Rules) != 3 {
		t.Fatalf("expected 3 rules, got %d", len(p.Rules))
	}
	if bps, ok := p.Rules[0].Then["rate_bps"].(int64); !ok || bps != 750 {
		t.Fatalf("expected rate_bps 750, got %v", p.Rules[0].Then["rate_bps"])
	}
	if !p.SubjectToRegazette {
		t.Fatal("expected subject_to_regazette true")
	}
}

func TestYAMLExemptBasketList(t *testing.T) {
	p, err := packFromYAML("rp-vat-exempt-basket", embeddedPacks["rp-vat-exempt-basket"], "embedded")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cats, ok := p.Rules[0].Then["categories"].([]any)
	if !ok || len(cats) == 0 {
		t.Fatalf("expected categories list, got %v", p.Rules[0].Then["categories"])
	}
}

func TestBasketClassification(t *testing.T) {
	ps := NewPackSet(Config{})
	ps.LoadPacks()
	cases := map[string]string{
		"basic_food":        "zero_rated",
		"pharmacy":          "zero_rated",
		"financial_services": "exempt",
		"residential_rent":  "exempt",
		"electronics":       "standard_75",
		"":                  "standard_75",
	}
	for cat, want := range cases {
		if got := ps.BasketFor(cat); got != want {
			t.Errorf("BasketFor(%q) = %s, want %s", cat, got, want)
		}
	}
	if ps.StandardRateBPS() != 750 {
		t.Errorf("standard rate = %d", ps.StandardRateBPS())
	}
}

func TestAttributionDualShadow(t *testing.T) {
	cfg := AttributionConfig{Mode: "dual_shadow", FederalShareBPS: 1000, StateShareBPS: 5500, LGAShareBPS: 3500}
	res := computeAttribution(10000, "Lagos", cfg)
	if res.FederalKobo != 1000 || res.StateKobo != 5500 {
		t.Errorf("primary attribution wrong: %+v", res)
	}
	if res.ShadowFederal != 10000 || res.ShadowState != 0 {
		t.Errorf("shadow attribution wrong: %+v", res)
	}
	cfg.Mode = "state"
	res = computeAttribution(10000, "Lagos", cfg)
	if res.FederalKobo != 1000 || res.StateKobo != 5500 || res.ShadowFederal != 0 {
		t.Errorf("state mode wrong: %+v", res)
	}
}

func TestEmbeddedGeo(t *testing.T) {
	g, err := EmbeddedGeo{}.AttributePoint(6.5244, 3.3792) // Lagos
	if err != nil {
		t.Fatalf("geo: %v", err)
	}
	if g.State != "Lagos" {
		t.Fatalf("expected Lagos, got %s", g.State)
	}
	if _, err := (EmbeddedGeo{}).AttributePoint(48.85, 2.35); err == nil {
		t.Fatal("expected out-of-bounds error for Paris")
	}
}

func TestDevLedgerSemantics(t *testing.T) {
	dl := NewDevLedger()
	float := accountID(LedgerVATRemittance, 99)
	pool := accountID(LedgerVATRemittance, NSVATFederalPool)
	dl.CreateAccounts([]LedgerAccount{{ID: float, Ledger: LedgerVATRemittance, Code: 1, Flags: "DEBITS_MUST_NOT_EXCEED_CREDITS"}})
	// float debits must not exceed credits
	if _, err := dl.Transfer(LedgerTransfer{DebitAccountID: float, CreditAccountID: pool, AmountKobo: 500, Ledger: 300, Code: 1}); err == nil {
		t.Fatal("expected float constraint violation")
	}
	// credit float first, then debit
	if _, err := dl.Transfer(LedgerTransfer{DebitAccountID: pool, CreditAccountID: float, AmountKobo: 1000, Ledger: 300, Code: 4}); err != nil {
		t.Fatalf("topup: %v", err)
	}
	if _, err := dl.Transfer(LedgerTransfer{DebitAccountID: float, CreditAccountID: pool, AmountKobo: 500, Ledger: 300, Code: 1}); err != nil {
		t.Fatalf("authorised debit within float: %v", err)
	}
	// pending -> post
	pid, err := dl.PendingTransfer(LedgerTransfer{DebitAccountID: float, CreditAccountID: pool, AmountKobo: 200, Ledger: 300, Code: 1})
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if _, err := dl.PostPending(pid, 200); err != nil {
		t.Fatalf("post: %v", err)
	}
	bal, err := dl.Balance(float)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.DebitsPosted != 700 || bal.CreditsPosted != 1000 {
		t.Fatalf("bad balance: %+v", bal)
	}
}

func TestProcessReceiptEndToEnd(t *testing.T) {
	cfg := Config{DataDir: t.TempDir(), AttributionMode: "state"}
	svc := NewService(cfg)
	svc.packs.LoadPacks()
	rc := &Receipt{
		TenantID: "t1", MerchantTIN: "12345678-0001", TerminalID: "POS-01", ReceiptNo: "R-1",
		Lat: 6.5244, Lon: 3.3792,
		Lines: []ReceiptLine{
			{SKU: "A", Qty: 1000, UnitPrice: 10000, Category: "electronics"}, // 100.00 NGN std
			{SKU: "B", Qty: 2000, UnitPrice: 5000, Category: "basic_food"},   // 100.00 NGN zero
		},
	}
	out, err := svc.processReceipt(rc, "k1")
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if out.TotalKobo != 20000 {
		t.Errorf("total = %d", out.TotalKobo)
	}
	if out.VATKobo != 750 { // 7.5% of 10000
		t.Errorf("vat = %d", out.VATKobo)
	}
	if out.State != "Lagos" {
		t.Errorf("state = %s", out.State)
	}
	if out.Baskets["zero_rated"] != 10000 || out.Baskets["standard_75"] != 10000 {
		t.Errorf("baskets = %+v", out.Baskets)
	}
	if out.Attribution.FederalKobo+out.Attribution.StateKobo > out.VATKobo {
		t.Errorf("attribution exceeds vat: %+v", out.Attribution)
	}
	// durability replay
	st2 := NewStore(cfg.DataDir)
	if _, ok := st2.GetReceipt(out.ID); !ok {
		t.Error("receipt not replayed from durable log")
	}
}

func TestULIDFormat(t *testing.T) {
	id := ULID()
	if len(id) != 26 {
		t.Fatalf("ulid len %d", len(id))
	}
	if !strings.ContainsAny(id, "0123456789ABCDEFGHJKMNPQRSTVWXYZ") {
		t.Fatal("bad alphabet")
	}
}
