package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// Regression (R4 S3#10): X-Api-Key previously granted operator on the whole
// mux — including POST /v1/apikeys, /rotate, /revoke — so a leaked machine
// key could mint unlimited keys and revoke the tenant's other keys. API-key
// auth is now scoped to invoice/VAT data endpoints; the key lifecycle is
// explicitly denied for key principals.
func TestAPIKeyDeniedOnKeyLifecycle(t *testing.T) {
	s := newKeyServer(t)
	rec := keyReq(s.handleAPIKeyCreate, "POST", "/v1/apikeys", "tenant-a", map[string]string{"name": "erp"})
	if rec.Code != 201 {
		t.Fatalf("create=%d", rec.Code)
	}
	plain, _ := decodeBody(t, rec)["api_key"].(string)

	// Wire the middleware as main.go does: a full mux behind it.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/apikeys", s.handleAPIKeyCreate)
	mux.HandleFunc("POST /v1/apikeys/{id}/rotate", s.handleAPIKeyRotate)
	mux.HandleFunc("POST /v1/apikeys/{id}/revoke", s.handleAPIKeyRevoke)
	mux.HandleFunc("GET /v1/apikeys", s.handleAPIKeyList)
	mux.HandleFunc("POST /v1/invoices", s.handleCreateInvoice)
	mux.HandleFunc("GET /v1/vat/summary", s.handleVATSummary)
	mux.HandleFunc("GET /v1/workflows", s.handleWorkflows)
	handler := s.apiKeyMiddleware(mux, mux) // fallback irrelevant here

	call := func(method, path string) *httptest.ResponseRecorder {
		var body *bytes.Reader
		if method == "POST" {
			raw, _ := json.Marshal(map[string]string{"name": "evil"})
			body = bytes.NewReader(raw)
		} else {
			body = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, body)
		req.Header.Set("X-Api-Key", plain)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	for _, p := range []struct{ method, path string }{
		{"POST", "/v1/apikeys"},
		{"GET", "/v1/apikeys"},
		{"POST", "/v1/apikeys/key_x/rotate"},
		{"POST", "/v1/apikeys/key_x/revoke"},
		{"GET", "/v1/workflows"}, // any other operator surface
	} {
		if rec := call(p.method, p.path); rec.Code != 403 {
			t.Errorf("api-key %s %s = %d, want 403", p.method, p.path, rec.Code)
		}
	}
}

// apiKeyAllowedPath: invoice/vat endpoints in, everything else out.
func TestAPIKeyAllowedPath(t *testing.T) {
	allowed := []string{"/v1/invoices", "/v1/invoices/nrs", "/v1/invoices/abc/qr", "/v1/b2c/report", "/v1/vat/summary"}
	denied := []string{"/v1/apikeys", "/v1/apikeys/key_1/rotate", "/v1/webhooks", "/v1/replay", "/v1/workflows", "/v1/apps", "/healthz"}
	for _, p := range allowed {
		if !apiKeyAllowedPath(p) {
			t.Errorf("path %s should be allowed for API keys", p)
		}
	}
	for _, p := range denied {
		if apiKeyAllowedPath(p) {
			t.Errorf("path %s must be denied for API keys", p)
		}
	}
}

// Regression (R4 S3#17): Verify was write-locked and rewrote the whole
// JSONL snapshot on EVERY call. It is now read-mostly: the store file is
// only rewritten when a key is mutated (or the usage stamp goes stale).
func TestAPIKeyVerifyIsReadMostly(t *testing.T) {
	dir := t.TempDir()
	store, err := NewAPIKeyStore(dir + "/keys.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	_, plain, err := store.Create("tenant-a", "erp", "tester")
	if err != nil {
		t.Fatal(err)
	}
	// First verification stamps LastUsedAt (nil -> now) and persists once.
	if _, ok := store.Verify(plain); !ok {
		t.Fatal("verify failed")
	}
	before, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	// Subsequent verifications inside the stamp interval: no rewrite.
	if _, ok := store.Verify(plain); !ok {
		t.Fatal("verify failed")
	}
	if _, ok := store.Verify(plain); !ok {
		t.Fatal("verify failed")
	}
	after, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("Verify rewrote the store snapshot on the read path")
	}
	// Stale stamp -> throttled persist resumes (behavioural correctness).
	k, _ := store.Verify(plain)
	stale := time.Now().UTC().Add(-2 * usageStampInterval)
	k.LastUsedAt = &stale
	if _, ok := store.Verify(plain); !ok {
		t.Fatal("verify with stale stamp failed")
	}
	after2, _ := os.ReadFile(store.path)
	if bytes.Equal(after, after2) {
		t.Fatal("stale usage stamp was not persisted")
	}
}
