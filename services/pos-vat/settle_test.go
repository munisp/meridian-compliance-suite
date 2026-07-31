package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// F5 helpers ---------------------------------------------------------------

func newSettleService(t *testing.T) *Service {
	t.Helper()
	cfg := Config{DataDir: t.TempDir(), AuthMode: "dev"}
	return NewService(cfg)
}

func addPeriodReceipt(t *testing.T, s *Service, id, period string, vat, federal, state int64) {
	t.Helper()
	rc := &Receipt{
		ID: id, TenantID: "tenant-f5", State: "Lagos",
		CapturedAt: period + "-15T10:00:00Z",
		VATKobo:    vat,
		Attribution: AttributionResult{
			Mode: "state", FederalKobo: federal, StateKobo: state,
		},
	}
	if err := s.store.PutReceipt(rc); err != nil {
		t.Fatal(err)
	}
}

func settleReq(t *testing.T, s *Service, period string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"period": period, "tenant_id": "tenant-f5"})
	req := httptest.NewRequest("POST", "/v1/settlement/recon", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleSettlementRecon(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func poolBal(t *testing.T, s *Service, ns uint64) int64 {
	t.Helper()
	b, err := s.ledger.Balance(accountID(LedgerVATRemittance, ns))
	if err != nil {
		t.Fatal(err)
	}
	return b.CreditsPosted
}

// TestSettleSamePeriodTwiceSinglePosting: settle the same period twice —
// the second run is a 200 no-op replay and the pools are credited exactly
// once (F5 / audit Flow 5).
func TestSettleSamePeriodTwiceSinglePosting(t *testing.T) {
	s := newSettleService(t)
	addPeriodReceipt(t, s, "rc-1", "2026-01", 100000, 10000, 55000)
	addPeriodReceipt(t, s, "rc-2", "2026-01", 50000, 5000, 27500)

	code, out := settleReq(t, s, "2026-01")
	if code != 201 {
		t.Fatalf("first settle: code=%d out=%v", code, out)
	}
	fed := poolBal(t, s, NSVATFederalPool)
	st := poolBal(t, s, NSVATStatePool)
	if fed != 15000 || st != 82500 {
		t.Fatalf("pools: federal=%d state=%d", fed, st)
	}

	code, out = settleReq(t, s, "2026-01")
	if code != 200 || out["idempotent_replay"] != true {
		t.Fatalf("re-settle must be 200 no-op replay, got code=%d out=%v", code, out)
	}
	if poolBal(t, s, NSVATFederalPool) != fed || poolBal(t, s, NSVATStatePool) != st {
		t.Fatal("re-settle must not double-post")
	}
	// settled_periods marker persisted
	sp := s.store.GetSettledPeriod("tenant-f5", "2026-01")
	if sp == nil || sp.Status != "settled" {
		t.Fatalf("settled marker: %+v", sp)
	}
}

// TestSettleCrashResume: crash after the marker+persisted pendings but
// before the posts — the next run resumes and settles exactly once.
func TestSettleCrashResume(t *testing.T) {
	s := newSettleService(t)
	addPeriodReceipt(t, s, "rc-3", "2026-02", 100000, 10000, 55000)
	merchant := accountID(LedgerVATRemittance, NSVATMerchant)
	if err := s.ledger.CreateAccounts([]LedgerAccount{{ID: merchant, Ledger: LedgerVATRemittance, Code: 4}}); err != nil {
		t.Fatal(err)
	}
	// simulate the crashed saga: pending marker + federal pending already
	// POSTED, state pending still open
	fedPend := DeterministicTransferID("posv-pend:tenant-f5:2026-02:federal")
	statePend := DeterministicTransferID("posv-pend:tenant-f5:2026-02:state")
	if _, err := s.ledger.PendingTransfer(LedgerTransfer{ID: fedPend, DebitAccountID: merchant,
		CreditAccountID: accountID(LedgerVATRemittance, NSVATFederalPool), AmountKobo: 10000, Ledger: LedgerVATRemittance, Code: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ledger.PostPending(fedPend, 10000); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SaveSettledPeriod(&SettledPeriod{
		TenantID: "tenant-f5", Period: "2026-02", FederalKobo: 10000, StateKobo: 55000,
		FederalPendingID: fedPend, StatePendingID: statePend, Status: "pending",
		UpdatedAt: nowRFC3339()}); err != nil {
		t.Fatal(err)
	}
	fedBefore := poolBal(t, s, NSVATFederalPool)
	code, out := settleReq(t, s, "2026-02")
	if code != 201 {
		t.Fatalf("resume settle: code=%d out=%v", code, out)
	}
	if poolBal(t, s, NSVATFederalPool) != fedBefore {
		t.Fatal("resume must not re-post the already-posted federal leg")
	}
	if poolBal(t, s, NSVATStatePool) != 55000 {
		t.Fatal("resume must post the state leg")
	}
	if sp := s.store.GetSettledPeriod("tenant-f5", "2026-02"); sp.Status != "settled" {
		t.Fatalf("marker: %+v", sp)
	}
}

// TestSettleCompensationPair: if the state leg cannot post, the already
// posted federal leg is reversed — the pair never splits.
func TestSettleCompensationPair(t *testing.T) {
	s := newSettleService(t)
	addPeriodReceipt(t, s, "rc-4", "2026-03", 100000, 10000, 55000)
	merchant := accountID(LedgerVATRemittance, NSVATMerchant)
	if err := s.ledger.CreateAccounts([]LedgerAccount{{ID: merchant, Ledger: LedgerVATRemittance, Code: 4}}); err != nil {
		t.Fatal(err)
	}
	// pre-create the state pending and VOID it so its post fails
	statePend := DeterministicTransferID("posv-pend:tenant-f5:2026-03:state")
	if _, err := s.ledger.PendingTransfer(LedgerTransfer{ID: statePend, DebitAccountID: merchant,
		CreditAccountID: accountID(LedgerVATRemittance, NSVATStatePool), AmountKobo: 55000, Ledger: LedgerVATRemittance, Code: 5}); err != nil {
		t.Fatal(err)
	}
	if err := s.ledger.VoidPending(statePend); err != nil {
		t.Fatal(err)
	}
	code, _ := settleReq(t, s, "2026-03")
	if code != 502 {
		t.Fatalf("settle must fail, got %d", code)
	}
	// federal leg posted then reversed: pool net zero
	fed := poolBal(t, s, NSVATFederalPool)
	fedBal, _ := s.ledger.Balance(accountID(LedgerVATRemittance, NSVATFederalPool))
	if fedBal.DebitsPosted != fed {
		t.Fatalf("federal leg must be reversed: posted=%d reversed=%d", fed, fedBal.DebitsPosted)
	}
	if sp := s.store.GetSettledPeriod("tenant-f5", "2026-03"); sp.Status != "failed" {
		t.Fatalf("marker: %+v", sp)
	}
}
