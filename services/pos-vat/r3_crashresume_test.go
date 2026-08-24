package main

// R3 verifier regression (pos-vat #40): one transient PostPending error
// triggered compensation that VOIDED the deterministic pending id; the
// retry then got "transfer id exists with different parameters or state"
// -> fell through to PostPending -> "not postable": the period was
// permanently 502-bricked (TestAdvCrashMidSettlementResume falsified
// crash-resume).
//
// Post-fix: a still-pending failed leg is never voided; the retry looks it
// up by deterministic id and posts it. Compensation (reversal of posted
// legs) only runs when the failed leg is no longer resumable.

import "testing"

// flakyLedger fails the Nth PostPending call once (transient downstream
// error), then delegates everything to the wrapped ledger.
type flakyLedger struct {
	inner   LedgerClient
	failOn  int
	calls   int
	failing bool
}

func (f *flakyLedger) CreateAccounts(a []LedgerAccount) error { return f.inner.CreateAccounts(a) }
func (f *flakyLedger) Transfer(t LedgerTransfer) (string, error) {
	return f.inner.Transfer(t)
}
func (f *flakyLedger) PendingTransfer(t LedgerTransfer) (string, error) {
	return f.inner.PendingTransfer(t)
}
func (f *flakyLedger) PostPending(id string, amount int64) (string, error) {
	f.calls++
	if f.failing && f.calls == f.failOn {
		f.failing = false // transient: succeeds next time
		return "", errTransientPost
	}
	return f.inner.PostPending(id, amount)
}
func (f *flakyLedger) VoidPending(id string) error      { return f.inner.VoidPending(id) }
func (f *flakyLedger) GetTransfer(id string) (*LedgerTransferState, error) {
	return f.inner.GetTransfer(id)
}
func (f *flakyLedger) Balance(id string) (*LedgerBalance, error) { return f.inner.Balance(id) }
func (f *flakyLedger) Mode() string                              { return f.inner.Mode() }

type transientError string

func (e transientError) Error() string { return string(e) }

const errTransientPost = transientError("transient downstream post error")

// TestCrashMidSettlementResume: transient PostPending error on the state
// leg, then retry — the retry must complete the settlement exactly once
// (pre-fix: 502 forever, pools stuck at 0/0).
func TestCrashMidSettlementResume(t *testing.T) {
	s := newSettleService(t)
	addPeriodReceipt(t, s, "rc-r3-1", "2026-06", 100000, 10000, 55000)
	merchant := accountID(LedgerVATRemittance, NSVATMerchant)
	if err := s.ledger.CreateAccounts([]LedgerAccount{{ID: merchant, Ledger: LedgerVATRemittance, Code: 4}}); err != nil {
		t.Fatal(err)
	}
	fl := &flakyLedger{inner: s.ledger, failOn: 2, failing: true} // state leg post
	s.ledger = fl

	code, out := settleReq(t, s, "2026-06")
	if code != 502 {
		t.Fatalf("first settle must fail on transient post error, got %d out=%v", code, out)
	}
	sp := s.store.GetSettledPeriod("tenant-f5", "2026-06")
	if sp == nil || sp.Status != "failed" {
		t.Fatalf("marker after transient failure: %+v", sp)
	}

	// retry: must resume and settle (pre-fix: 502 "not postable" forever)
	code, out = settleReq(t, s, "2026-06")
	if code != 201 {
		t.Fatalf("resume settle: code=%d out=%v (period bricked?)", code, out)
	}
	if poolBal(t, s, NSVATFederalPool) != 10000 {
		t.Fatalf("federal pool = %d, want 10000", poolBal(t, s, NSVATFederalPool))
	}
	if poolBal(t, s, NSVATStatePool) != 55000 {
		t.Fatalf("state pool = %d, want 55000", poolBal(t, s, NSVATStatePool))
	}
	sp = s.store.GetSettledPeriod("tenant-f5", "2026-06")
	if sp.Status != "settled" {
		t.Fatalf("marker after resume: %+v", sp)
	}

	// a third settle is a no-op replay — no double posting
	code, out = settleReq(t, s, "2026-06")
	if code != 200 || out["idempotent_replay"] != true {
		t.Fatalf("re-settle after resume must be replay, got %d out=%v", code, out)
	}
	if poolBal(t, s, NSVATFederalPool) != 10000 || poolBal(t, s, NSVATStatePool) != 55000 {
		t.Fatal("double posting after resume")
	}
}

// TestTransientFailFirstLegResume: same guarantee when the FIRST leg's
// post fails transiently (nothing posted yet, all legs resume).
func TestTransientFailFirstLegResume(t *testing.T) {
	s := newSettleService(t)
	addPeriodReceipt(t, s, "rc-r3-2", "2026-07", 100000, 10000, 55000)
	merchant := accountID(LedgerVATRemittance, NSVATMerchant)
	if err := s.ledger.CreateAccounts([]LedgerAccount{{ID: merchant, Ledger: LedgerVATRemittance, Code: 4}}); err != nil {
		t.Fatal(err)
	}
	fl := &flakyLedger{inner: s.ledger, failOn: 1, failing: true} // federal leg post
	s.ledger = fl
	if code, out := settleReq(t, s, "2026-07"); code != 502 {
		t.Fatalf("first settle: code=%d out=%v", code, out)
	}
	code, out := settleReq(t, s, "2026-07")
	if code != 201 {
		t.Fatalf("resume: code=%d out=%v", code, out)
	}
	if poolBal(t, s, NSVATFederalPool) != 10000 || poolBal(t, s, NSVATStatePool) != 55000 {
		t.Fatalf("pools after resume: fed=%d state=%d",
			poolBal(t, s, NSVATFederalPool), poolBal(t, s, NSVATStatePool))
	}
}
