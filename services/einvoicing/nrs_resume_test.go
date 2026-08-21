package main

// nrs_resume_test.go — FF-6 interrupted e-invoice recovery: a crash mid-flow
// leaves a durable "signed"/"transmitted" record; the fix must (a) resume the
// lifecycle on client retry instead of returning the stale record, and
// (b) sweep interrupted invoices to "confirmed" exactly once, never issuing
// a second IRN or a duplicate invoice.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// crashAfterSaves arms a store fault hook that fails saves [from, to],
// simulating a process crash mid-workflow: the fault also swallows nrsFail's
// status write, so the last durable state stays mid-flow (no "failed").
func crashAfterSaves(srv *Server, saves *atomic.Int32, from, to int32) {
	srv.store.setFaultHook(func(op string) error {
		if n := saves.Add(1); n >= from && n <= to {
			return errSimDeadlock
		}
		return nil
	})
}

// Crash during transmit (step 7): draft save (1) and sign save (2) land;
// transmit saves (3-5) and the nrsFail save (6) are lost -> durable "signed".
func TestNRSResumeOnRetryAfterMidFlowCrash(t *testing.T) {
	dir := t.TempDir()
	sink := &InprocWebhookSink{}
	srv1, mux1 := newNRSTestServerAt(t, dir, sink)
	n := sampleNRSPayload()
	n.IRN = "INVRES1-94ND90NR-20260127"
	if err := srv1.webhooks.Register(n.BusinessID, "https://stakeholder/hook", "sekret"); err != nil {
		t.Fatal(err)
	}
	var saves atomic.Int32
	crashAfterSaves(srv1, &saves, 3, 6)
	rec := postNRS(t, mux1, n)
	if rec.Code == 201 && decodeNRSResp(t, rec).Status == "confirmed" {
		t.Fatalf("expected mid-flow crash, got %d %s", rec.Code, rec.Body)
	}
	stored1, ok := srv1.store.GetByIRN(n.IRN)
	if !ok || stored1.Status != "signed" {
		t.Fatalf("durable state after crash = %+v ok=%v, want signed", stored1, ok)
	}

	// restart: fresh server over the same durable dir, healthy rails
	srv2, mux2 := newNRSTestServerAt(t, dir, &InprocWebhookSink{})
	if err := srv2.webhooks.Register(n.BusinessID, "https://stakeholder/hook", "sekret"); err != nil {
		t.Fatal(err)
	}

	// BEFORE the fix this replay returned the stale "signed" record and the
	// invoice never converged. Now the retry must resume the lifecycle.
	rec2 := postNRS(t, mux2, n)
	r2 := decodeNRSResp(t, rec2)
	if rec2.Code != 200 || !r2.IdempotentReplay {
		t.Fatalf("resume replay: %d %s", rec2.Code, rec2.Body)
	}
	if r2.Status != "confirmed" || r2.IRN != n.IRN {
		t.Fatalf("after resume: %+v, want confirmed irn=%s", r2, n.IRN)
	}
	if got := irnCount(srv2, n.IRN); got != 1 {
		t.Fatalf("duplicate invoices for IRN: %d", got)
	}

	// concurrent retries: the in-flight guard serializes drivers; exactly one
	// invoice, confirmed exactly once, IRN stable.
	const parallel = 8
	var wg sync.WaitGroup
	codes := make([]int, parallel)
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = postNRS(t, mux2, n).Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("concurrent retry %d: code %d", i, c)
		}
	}
	final, _ := srv2.store.GetByIRN(n.IRN)
	if final.Status != "confirmed" || final.IRN != n.IRN {
		t.Fatalf("final: %+v", final)
	}
	if got := irnCount(srv2, n.IRN); got != 1 {
		t.Fatalf("duplicates after concurrent retries: %d", got)
	}
	confirmed := 0
	for _, inv := range srv2.store.List() {
		if inv.IRN == n.IRN && inv.Status == "confirmed" {
			confirmed++
		}
	}
	if confirmed != 1 {
		t.Fatalf("confirmed invoices = %d, want exactly 1", confirmed)
	}
}

// Crash during confirm (step 8): saves through transmit (1-3) land; the
// confirm saves (4-6) and nrsFail save (7) are lost -> durable "transmitted".
// The recovery sweep (no client action) must drive it to "confirmed".
func TestNRSRecoverySweepConfirmsInterrupted(t *testing.T) {
	dir := t.TempDir()
	srv1, mux1 := newNRSTestServerAt(t, dir, &InprocWebhookSink{})
	n := sampleNRSPayload()
	n.IRN = "INVSWP1-94ND90NR-20260127"
	if err := srv1.webhooks.Register(n.BusinessID, "https://stakeholder/hook", "sekret"); err != nil {
		t.Fatal(err)
	}
	var saves atomic.Int32
	crashAfterSaves(srv1, &saves, 4, 7)
	rec := postNRS(t, mux1, n)
	if rec.Code == 201 && decodeNRSResp(t, rec).Status == "confirmed" {
		t.Fatalf("expected mid-flow crash, got %d %s", rec.Code, rec.Body)
	}
	stored1, ok := srv1.store.GetByIRN(n.IRN)
	if !ok || stored1.Status != "transmitted" {
		t.Fatalf("durable state after crash = %+v ok=%v, want transmitted", stored1, ok)
	}

	// restart and sweep — no client retry involved
	srv2, _ := newNRSTestServerAt(t, dir, &InprocWebhookSink{})
	if err := srv2.webhooks.Register(n.BusinessID, "https://stakeholder/hook", "sekret"); err != nil {
		t.Fatal(err)
	}
	if n2 := srv2.RecoverInterruptedNRS(context.Background()); n2 != 1 {
		t.Fatalf("sweep resumed %d invoices, want 1", n2)
	}
	stored2, _ := srv2.store.GetByIRN(n.IRN)
	if stored2.Status != "confirmed" || stored2.IRN != n.IRN {
		t.Fatalf("after sweep: %+v, want confirmed irn=%s", stored2, n.IRN)
	}
	if got := irnCount(srv2, n.IRN); got != 1 {
		t.Fatalf("duplicate invoices after sweep: %d", got)
	}
	// sweep is idempotent on terminal records: a second pass resumes nothing.
	if n3 := srv2.RecoverInterruptedNRS(context.Background()); n3 != 0 {
		t.Fatalf("second sweep resumed %d, want 0", n3)
	}
	if got := irnCount(srv2, n.IRN); got != 1 {
		t.Fatalf("duplicate invoices after second sweep: %d", got)
	}
}

// nrsInterimStatus classification: only mid-flow NRS records are resumable;
// terminal and non-NRS records are untouched.
func TestNRSInterimStatusClassification(t *testing.T) {
	cases := []struct {
		status  string
		payload string
		want    bool
	}{
		{"received", "{}", true},
		{"signed", "{}", true},
		{"transmitted", "{}", true},
		{"confirmed", "{}", false},
		{"failed", "{}", false},     // explicit retry-exhausted outcome
		{"signed", "", false},       // not NRS-flow
		{"precleared", "", false},   // MBS flow
		{"", "{}", false},           // defensive: unknown status
	}
	for i, tc := range cases {
		inv := &CanonicalInvoice{Status: tc.status, NRSPayload: tc.payload}
		if got := nrsInterimStatus(inv); got != tc.want {
			t.Errorf("case %d (%q,%q): got %v want %v", i, tc.status, tc.payload, got, tc.want)
		}
	}
	if nrsInterimStatus(nil) {
		t.Error("nil invoice must not be interim")
	}
}
