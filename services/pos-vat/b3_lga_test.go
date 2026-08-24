package main

import "testing"

// B3 #2 regression: POS-VAT settlement must conserve the full vat_kobo —
// federal + state + LGA legs sum to 100% — and receipts arriving after a
// period was settled must be carried into a supplemental settlement, never
// silently dropped.
//
// Pre-fix: only federal+state legs post (35% LGA share vanishes) and the
// settled-period replay short-circuits late receipts.

// Hand-check from the audit: VAT ₦750.00 (75,000 kobo) at 10%/55%/35%
// must settle 7,500 + 41,250 + 26,250 = 75,000 kobo.
func TestB3AttributionConservesFullVAT(t *testing.T) {
	acfg := AttributionConfig{Mode: "state", FederalShareBPS: 1000, StateShareBPS: 5500, LGAShareBPS: 3500}
	res := computeAttribution(75000, "Lagos", acfg)
	if res.FederalKobo != 7500 || res.StateKobo != 41250 || res.LGAKobo != 26250 {
		t.Fatalf("attribution = fed %d state %d lga %d; want 7500/41250/26250",
			res.FederalKobo, res.StateKobo, res.LGAKobo)
	}
	if res.FederalKobo+res.StateKobo+res.LGAKobo != 75000 {
		t.Fatal("legs do not sum to full vat_kobo")
	}
	// odd-amount rounding must still conserve (remainder allocation)
	for _, vat := range []int64{1, 3, 7, 101, 99999, 123456789} {
		r := computeAttribution(vat, "Lagos", acfg)
		if r.FederalKobo+r.StateKobo+r.LGAKobo != vat {
			t.Fatalf("vat %d: legs sum %d", vat, r.FederalKobo+r.StateKobo+r.LGAKobo)
		}
	}
}

// Settlement must post THREE legs summing to the full VAT of the period.
func TestB3SettlePostsLGALeg(t *testing.T) {
	s := newSettleService(t)
	addPeriodReceipt(t, s, "rc-lga-1", "2026-02", 75000, 7500, 41250)
	code, _ := settleReq(t, s, "2026-02")
	if code != 201 {
		t.Fatalf("settle code %d", code)
	}
	fed := poolBal(t, s, NSVATFederalPool)
	state := poolBal(t, s, NSVATStatePool)
	lga := poolBal(t, s, NSVATLGAPool)
	if fed != 7500 || state != 41250 || lga != 26250 {
		t.Fatalf("pools fed %d state %d lga %d; want 7500/41250/26250", fed, state, lga)
	}
	if fed+state+lga != 75000 {
		t.Fatal("settled legs do not conserve full vat_kobo")
	}
}

// A receipt ingested AFTER its period was settled must be remitted by a
// later settle run (supplemental carry-over), never dropped.
func TestB3LateReceiptCarryOverSettled(t *testing.T) {
	s := newSettleService(t)
	addPeriodReceipt(t, s, "rc-early", "2026-09", 100000, 10000, 55000)
	code, _ := settleReq(t, s, "2026-09")
	if code != 201 {
		t.Fatalf("first settle code %d", code)
	}
	// late receipt arrives in the already-settled period
	addPeriodReceipt(t, s, "rc-late", "2026-09", 75000, 7500, 41250)
	code, out := settleReq(t, s, "2026-09")
	if code == 200 && out["idempotent_replay"] == true {
		t.Fatal("late receipt silently dropped by settled-period replay")
	}
	fed := poolBal(t, s, NSVATFederalPool)
	state := poolBal(t, s, NSVATStatePool)
	lga := poolBal(t, s, NSVATLGAPool)
	total := fed + state + lga
	if total != 175000 {
		t.Fatalf("settled total %d; want 175000 (both receipts remitted)", total)
	}
	if lga != 61250 {
		t.Fatalf("lga pool %d; want 61250 (35000+26250)", lga)
	}
	// a third run with no new receipts is a clean no-op replay
	code, out = settleReq(t, s, "2026-09")
	if code != 200 || out["idempotent_replay"] != true {
		t.Fatalf("expected replay, got %d %v", code, out)
	}
	if poolBal(t, s, NSVATFederalPool)+poolBal(t, s, NSVATStatePool)+poolBal(t, s, NSVATLGAPool) != 175000 {
		t.Fatal("replay double-posted")
	}
}
