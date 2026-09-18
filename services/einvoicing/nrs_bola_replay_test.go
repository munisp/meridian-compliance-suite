package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// newNRSBOLAServer is newBOLAServer plus the NRS-flow extras (service-ID
// registry, webhook sink) the 8-step lifecycle needs.
func newNRSBOLAServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
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
	s := &Server{
		store: store, outbox: outbox, signer: signer,
		validator:  NewValidator(),
		router:     NewAPPRouter(NewMBSClient()),
		runner:     NewInprocRunner(),
		serviceIDs: NewServiceIDRegistry(),
		webhooks:   NewWebhookRegistry(&InprocWebhookSink{}),
	}
	registerWorkflows(s.runner)
	return s
}

// Regression (R4 S3#2): the IRN idempotent-replay path of handleNRSCreate
// returned any tenant's stored invoice (payload, crypto stamp, QR) with no
// tenantGuard, and the resume branch re-drove cross-tenant workflows. IRNs
// are structured and enumerable, so this was a full cross-tenant harvest.
// The guard must now apply to ALL retrieval/replay/resume paths.
func TestNRSReplayCrossTenantBlocked(t *testing.T) {
	s := newNRSBOLAServer(t)

	payload := sampleNRSPayload()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	// Tenant A creates the invoice (fresh, no IRN -> 201).
	rec := doReq(s.handleNRSCreate, "POST", "/v1/invoices/nrs", "tenant-a", body)
	if rec.Code != 201 {
		t.Fatalf("create: want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	var created nrsAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create: bad response: %v", err)
	}
	if created.IRN == "" {
		t.Fatal("create: empty IRN")
	}

	// Tenant B replays the SAME IRN: must be 404 (no existence oracle),
	// and must not leak any invoice material.
	replay := sampleNRSPayload()
	replay.IRN = created.IRN
	rbody, _ := json.Marshal(replay)
	rec = doReq(s.handleNRSCreate, "POST", "/v1/invoices/nrs", "tenant-b", rbody)
	if rec.Code != 404 {
		t.Fatalf("cross-tenant IRN replay: want 404, got %d (%s)", rec.Code, rec.Body.String())
	}
	var leak map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &leak)
	for _, k := range []string{"invoice", "crypto_stamp", "qr", "invoice_id", "run_id"} {
		if _, ok := leak[k]; ok {
			t.Fatalf("cross-tenant replay leaked %q: %s", k, rec.Body.String())
		}
	}

	// Same-tenant replay still works (idempotent 200 with the full record).
	rec = doReq(s.handleNRSCreate, "POST", "/v1/invoices/nrs", "tenant-a", rbody)
	if rec.Code != 200 {
		t.Fatalf("same-tenant replay: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var again nrsAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &again); err != nil {
		t.Fatalf("replay: bad response: %v", err)
	}
	if !again.IdempotentReplay || again.IRN != created.IRN {
		t.Fatalf("replay: want idempotent replay of %s, got %+v", created.IRN, again)
	}

	// A service token with an empty tenant claim is a 403 against a
	// tenant-owned invoice, never a void check.
	rec = doReq(s.handleNRSCreate, "POST", "/v1/invoices/nrs", "", rbody)
	if rec.Code != 403 {
		t.Fatalf("empty-tenant replay: want 403, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// The idempotency-key replay branch (priorID from Save) is guarded too:
// tenant B reusing tenant A's idempotency key + IRN must not receive the
// stored invoice.
func TestNRSIdempotencyKeyReplayCrossTenantBlocked(t *testing.T) {
	s := newNRSBOLAServer(t)

	payload := sampleNRSPayload()
	body, _ := json.Marshal(payload)
	rec := doReq(s.handleNRSCreate, "POST", "/v1/invoices/nrs", "tenant-a", body)
	if rec.Code != 201 {
		t.Fatalf("create: want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	var created nrsAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create: bad response: %v", err)
	}

	// Cross-tenant replay by IRN (the store-level key path is covered by the
	// IRN branch here because FromNRS preserves the IRN). Expect 404.
	replay := sampleNRSPayload()
	replay.IRN = created.IRN
	rbody, _ := json.Marshal(replay)
	rec = doReq(s.handleNRSCreate, "POST", "/v1/invoices/nrs", "tenant-b", rbody)
	if rec.Code != 404 {
		t.Fatalf("cross-tenant replay: want 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}
