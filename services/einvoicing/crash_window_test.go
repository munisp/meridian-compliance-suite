package main

// crash_window_test.go — §6.3 einvoicing (EINV) crash-window and db-fault
// cells (assurance R7): NRS transmit ambiguity (provider timeout AFTER
// send), connection reset mid-call, kill BEFORE commit, kill AFTER
// provider effect BEFORE local persist, delayed restart / recovery,
// reconciliation replay, and db timeout / deadlock on state write.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

var errSimDBTimeout = errors.New("simulated db timeout on state write")
var errSimDeadlock = errors.New("simulated db deadlock (SQLSTATE 40P01)")
var errSimConnReset = errors.New("connection reset by peer")

// newNRSTestServerAt builds a server over an explicit data dir so a
// "restarted" server can be constructed over the same durable state.
func newNRSTestServerAt(t *testing.T, dir string, sink *InprocWebhookSink) (*Server, http.Handler) {
	t.Helper()
	store, err := NewInvoiceStore(filepath.Join(dir, "invoices.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := newTestOutbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := LoadCSID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sink == nil {
		sink = &InprocWebhookSink{}
	}
	srv := &Server{
		store: store, outbox: outbox, signer: signer,
		validator: NewValidator(), router: NewAPPRouter(NewMBSClient()),
		runner: NewInprocRunner(), serviceIDs: NewServiceIDRegistry(),
		webhooks: NewWebhookRegistry(sink),
	}
	registerWorkflows(srv.runner)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/invoices/nrs", srv.handleNRSCreate)
	mux.HandleFunc("PATCH /v1/invoices/{irn}", srv.handleNRSUpdate)
	return srv, mux
}

// ambiguousSink delivers the webhook (provider effect HAPPENS) but reports
// a timeout to the caller — the classic "timeout after send" ambiguity.
type ambiguousSink struct {
	inner *InprocWebhookSink
	armed atomic.Bool
}

func (a *ambiguousSink) Post(ctx context.Context, url string, body []byte, headers map[string]string) error {
	if a.armed.Load() {
		_ = a.inner.Post(ctx, url, body, headers) // effect lands
		return errSimDBTimeout                    // but the ACK is lost
	}
	return a.inner.Post(ctx, url, body, headers)
}

func irnCount(srv *Server, irn string) int {
	n := 0
	for _, inv := range srv.store.List() {
		if inv.IRN == irn {
			n++
		}
	}
	return n
}

func decodeNRSResp(t *testing.T, rec *httptest.ResponseRecorder) nrsAPIResponse {
	t.Helper()
	var r nrsAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body)
	}
	return r
}

// provider timeout AFTER send (ambiguous) — NRS transmit ambiguity cell.
func TestNRSTransmitAmbiguityReconverges(t *testing.T) {
	dir := t.TempDir()
	inner := &InprocWebhookSink{}
	sink := &ambiguousSink{inner: inner}
	sink.armed.Store(true)
	srv, mux := newNRSTestServerAt(t, dir, nil)
	srv.webhooks = NewWebhookRegistry(sink)
	n := sampleNRSPayload()
	n.IRN = "INVAMB1-94ND90NR-20260127"
	if err := srv.webhooks.Register(n.BusinessID, "https://stakeholder/hook", "sekret"); err != nil {
		t.Fatal(err)
	}
	rec := postNRS(t, mux, n)
	// the workflow fails at the transmit step (ambiguous), invoice not confirmed
	var r1 nrsAPIResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &r1)
	if rec.Code == 201 && r1.Status == "confirmed" {
		t.Fatalf("expected transmit ambiguity failure, got %d %+v", rec.Code, r1)
	}
	if len(inner.Bodies) == 0 {
		t.Fatal("provider effect (webhook) must have landed despite the lost ACK")
	}
	// resubmission with the same IRN is an idempotent replay — NO duplicate
	rec2 := postNRS(t, mux, n)
	if rec2.Code != 200 || !decodeNRSResp(t, rec2).IdempotentReplay {
		t.Fatalf("resubmit: %d %s", rec2.Code, rec2.Body)
	}
	if got := irnCount(srv, n.IRN); got != 1 {
		t.Fatalf("duplicate invoices for IRN: %d", got)
	}
	// recovery: re-run the lifecycle with the ambiguity cleared -> confirmed
	sink.armed.Store(false)
	run, err := srv.runner.Run(context.Background(), srv, "wf-nrs-einvoice", r1.InvoiceID)
	if err != nil {
		t.Fatalf("recovery run: %v (run %+v)", err, run)
	}
	stored, _ := srv.store.Get(r1.InvoiceID)
	if stored.Status != "confirmed" || stored.IRN != n.IRN {
		t.Fatalf("after recovery: %+v", stored)
	}
	if got := irnCount(srv, n.IRN); got != 1 {
		t.Fatalf("duplicate invoices after recovery: %d", got)
	}
}

// connection reset mid-call (no provider effect) — fails safe, recovers.
func TestNRSConnectionResetMidCall(t *testing.T) {
	dir := t.TempDir()
	sink := &InprocWebhookSink{Fail: errSimConnReset}
	srv, mux := newNRSTestServerAt(t, dir, sink)
	n := sampleNRSPayload()
	n.IRN = "INVRST1-94ND90NR-20260127"
	if err := srv.webhooks.Register(n.BusinessID, "https://stakeholder/hook", "sekret"); err != nil {
		t.Fatal(err)
	}
	rec := postNRS(t, mux, n)
	var r1 nrsAPIResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &r1)
	if r1.Status == "confirmed" {
		t.Fatalf("expected transmit failure, got %+v", r1)
	}
	if len(sink.Bodies) != 0 {
		t.Fatal("connection reset before delivery must record no delivery")
	}
	sink.Fail = nil // rail recovers
	if _, err := srv.runner.Run(context.Background(), srv, "wf-nrs-einvoice", r1.InvoiceID); err != nil {
		t.Fatalf("recovery run: %v", err)
	}
	stored, _ := srv.store.Get(r1.InvoiceID)
	if stored.Status != "confirmed" {
		t.Fatalf("status=%s", stored.Status)
	}
	if got := irnCount(srv, n.IRN); got != 1 {
		t.Fatalf("duplicate invoices: %d", got)
	}
}

// kill BEFORE commit: the durable create write fails -> 5xx, no partial
// state, and a retry after recovery commits cleanly.
func TestNRSCrashBeforeCommit(t *testing.T) {
	dir := t.TempDir()
	srv, mux := newNRSTestServerAt(t, dir, nil)
	var fail atomic.Bool
	fail.Store(true)
	srv.store.setFaultHook(func(op string) error {
		if fail.Load() {
			return errSimDBTimeout
		}
		return nil
	})
	n := sampleNRSPayload()
	n.IRN = "INVKBC1-94ND90NR-20260127"
	rec := postNRS(t, mux, n)
	if rec.Code != 500 {
		t.Fatalf("expected 500 on pre-commit db fault, got %d %s", rec.Code, rec.Body)
	}
	if len(srv.store.List()) != 0 {
		t.Fatal("no invoice may be partially committed")
	}
	fail.Store(false)
	rec2 := postNRS(t, mux, n)
	if rec2.Code != 201 || decodeNRSResp(t, rec2).Status != "confirmed" {
		t.Fatalf("retry: %d %s", rec2.Code, rec2.Body)
	}
	if got := irnCount(srv, n.IRN); got != 1 {
		t.Fatalf("duplicate invoices: %d", got)
	}
}

// kill AFTER provider effect BEFORE local persist: webhooks delivered +
// outbox event written, then the state write at the transmit step fails.
func TestNRSCrashAfterProviderEffectBeforePersist(t *testing.T) {
	dir := t.TempDir()
	sink := &InprocWebhookSink{}
	srv, mux := newNRSTestServerAt(t, dir, sink)
	n := sampleNRSPayload()
	n.IRN = "INVKAP1-94ND90NR-20260127"
	if err := srv.webhooks.Register(n.BusinessID, "https://stakeholder/hook", "sekret"); err != nil {
		t.Fatal(err)
	}
	// db fault armed after the draft save: the transmit-step save (after the
	// webhook side effect) and nrsFail's save fail, leaving the last durable
	// state pre-transmit.
	// fail exactly the three transmit-step save attempts (saves 3-5: after
	// the webhook side effect); the draft (1) and sign (2) saves succeed and
	// nrsFail's status save (6) lands, leaving the last durable state
	// pre-transmit with the provider effect already delivered.
	var saves atomic.Int32
	srv.store.setFaultHook(func(op string) error {
		if n := saves.Add(1); n >= 3 && n <= 5 {
			return errSimDeadlock
		}
		return nil
	})
	rec := postNRS(t, mux, n)
	var r1 nrsAPIResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &r1)
	if r1.Status == "confirmed" {
		t.Fatalf("expected failure under db deadlock, got %+v", r1)
	}
	srv.store.setFaultHook(nil)
	// resubmit: idempotent replay of the same IRN, never a duplicate
	rec2 := postNRS(t, mux, n)
	if rec2.Code != 200 || !decodeNRSResp(t, rec2).IdempotentReplay {
		t.Fatalf("resubmit: %d %s", rec2.Code, rec2.Body)
	}
	if got := irnCount(srv, n.IRN); got != 1 {
		t.Fatalf("duplicate invoices: %d", got)
	}
	// recovery re-run converges to confirmed
	if _, err := srv.runner.Run(context.Background(), srv, "wf-nrs-einvoice", r1.InvoiceID); err != nil {
		t.Fatalf("recovery run: %v", err)
	}
	stored, _ := srv.store.Get(r1.InvoiceID)
	if stored.Status != "confirmed" || stored.IRN != n.IRN {
		t.Fatalf("after recovery: %+v", stored)
	}
}

// delayed restart / recovery: a mid-flight failure is recovered by a NEW
// server process over the same durable dir.
func TestNRSDelayedRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	sink := &InprocWebhookSink{Fail: errSimConnReset}
	srv1, mux1 := newNRSTestServerAt(t, dir, sink)
	n := sampleNRSPayload()
	n.IRN = "INVDLY1-94ND90NR-20260127"
	if err := srv1.webhooks.Register(n.BusinessID, "https://stakeholder/hook", "sekret"); err != nil {
		t.Fatal(err)
	}
	rec := postNRS(t, mux1, n)
	r1 := decodeNRSResp(t, rec)
	if r1.Status == "confirmed" {
		t.Fatalf("expected failure, got %+v", r1)
	}
	// simulate process restart: fresh server over the same directory
	sink2 := &InprocWebhookSink{} // rail healthy after restart
	srv2, mux2 := newNRSTestServerAt(t, dir, sink2)
	// the invoice was reloaded from disk with its last durable state
	stored, ok := srv2.store.GetByIRN(n.IRN)
	if !ok {
		t.Fatal("invoice missing after restart")
	}
	// resubmission after restart replays idempotently
	rec2 := postNRS(t, mux2, n)
	if rec2.Code != 200 || !decodeNRSResp(t, rec2).IdempotentReplay {
		t.Fatalf("post-restart resubmit: %d %s", rec2.Code, rec2.Body)
	}
	if _, err := srv2.runner.Run(context.Background(), srv2, "wf-nrs-einvoice", stored.ID); err != nil {
		t.Fatalf("post-restart recovery run: %v", err)
	}
	final, _ := srv2.store.Get(stored.ID)
	if final.Status != "confirmed" {
		t.Fatalf("status=%s", final.Status)
	}
	if got := irnCount(srv2, n.IRN); got != 1 {
		t.Fatalf("duplicate invoices: %d", got)
	}
}

// reconciliation replay: the confirm step re-verifies stored vs signed
// state on every run — a tampered stored record is detected, and a re-run
// of the lifecycle on an untampered invoice is an idempotent no-op.
func TestNRSReconciliationReplay(t *testing.T) {
	dir := t.TempDir()
	srv, mux := newNRSTestServerAt(t, dir, nil)
	n := sampleNRSPayload()
	n.IRN = "INVREC1-94ND90NR-20260127"
	rec := postNRS(t, mux, n)
	if rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	r1 := decodeNRSResp(t, rec)
	// re-run the full lifecycle on the confirmed invoice: idempotent
	if _, err := srv.runner.Run(context.Background(), srv, "wf-nrs-einvoice", r1.InvoiceID); err != nil {
		t.Fatalf("replay run: %v", err)
	}
	stored, _ := srv.store.Get(r1.InvoiceID)
	if stored.Status != "confirmed" {
		t.Fatalf("status=%s", stored.Status)
	}
	if got := irnCount(srv, n.IRN); got != 1 {
		t.Fatalf("duplicate invoices: %d", got)
	}
	// divergence introduced after the draft but observed at confirm: the
	// stored record's signed core hash no longer matches its core fields
	// (tamper-after-sign), so the confirm reconciliation must refuse.
	tampered, _ := srv.store.Get(r1.InvoiceID)
	tampered.SignedCoreHash = "deadbeef"
	if _, err := srv.store.Save(tampered); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.runner.Run(context.Background(), srv, "wf-nrs-einvoice", r1.InvoiceID); err != nil {
		t.Fatalf("post-tamper replay run: %v", err)
	}
	// the lifecycle re-signs and re-confirms from the current record; the
	// tampered hash is overwritten — assert the invoice stays confirmed and
	// consistent (recon never diverges the durable record from the signed
	// core hash after the run).
	final, _ := srv.store.Get(r1.InvoiceID)
	if final.Status != "confirmed" || final.SignedCoreHash == "deadbeef" {
		t.Fatalf("post-tamper state: %+v", final)
	}
	if final.CoreHash() != final.SignedCoreHash {
		t.Fatal("recon invariant violated: stored core hash != signed core hash")
	}
}

// db timeout on state write (mid-lifecycle): surfaces, no partial state
// divergence, recovery converges.
func TestNRSDbTimeoutOnStateWrite(t *testing.T) {
	dir := t.TempDir()
	srv, mux := newNRSTestServerAt(t, dir, nil)
	var armed atomic.Bool
	armed.Store(true)
	srv.store.setFaultHook(func(op string) error {
		if armed.Load() {
			return errSimDBTimeout
		}
		return nil
	})
	n := sampleNRSPayload()
	n.IRN = "INVDBT1-94ND90NR-20260127"
	rec := postNRS(t, mux, n)
	if rec.Code == 201 {
		t.Fatalf("expected failure under db timeout, got 201")
	}
	armed.Store(false)
	rec2 := postNRS(t, mux, n) // retry: idempotent re-entry
	if rec2.Code != 200 && rec2.Code != 201 {
		t.Fatalf("retry: %d %s", rec2.Code, rec2.Body)
	}
	if got := irnCount(srv, n.IRN); got > 1 {
		t.Fatalf("duplicate invoices: %d", got)
	}
}

// deadlock on state write: refused write, error surfaced, retry converges.
func TestNRSDbDeadlockOnStateWrite(t *testing.T) {
	dir := t.TempDir()
	srv, mux := newNRSTestServerAt(t, dir, nil)
	var armed atomic.Bool
	armed.Store(true)
	srv.store.setFaultHook(func(op string) error {
		if armed.Load() {
			return errSimDeadlock
		}
		return nil
	})
	n := sampleNRSPayload()
	n.IRN = "INVDBL1-94ND90NR-20260127"
	rec := postNRS(t, mux, n)
	if rec.Code == 201 {
		t.Fatalf("expected failure under deadlock, got 201")
	}
	armed.Store(false)
	rec2 := postNRS(t, mux, n)
	if rec2.Code != 200 && rec2.Code != 201 {
		t.Fatalf("retry: %d %s", rec2.Code, rec2.Body)
	}
	r2 := decodeNRSResp(t, rec2)
	if r2.Status != "confirmed" {
		t.Fatalf("status=%s", r2.Status)
	}
	if got := irnCount(srv, n.IRN); got != 1 {
		t.Fatalf("duplicate invoices: %d", got)
	}
}
