package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/munisp/meridian-compliance-suite/packages/shared/devjwt"
)

// newKeyServer builds a Server with an APIKeyStore for lifecycle tests.
func newKeyServer(t *testing.T) *Server {
	t.Helper()
	s := newBOLAServer(t)
	ks, err := NewAPIKeyStore(t.TempDir() + "/apikeys.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	s.apiKeys = ks
	return s
}

// keyReq issues an authenticated lifecycle request (admin role, tenant).
func keyReq(h http.HandlerFunc, method, path, tenant string, body any, pathVals ...string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("X-Dev-Role", "admin")
	if tenant != "" {
		req.Header.Set("X-Tenant-ID", tenant)
	}
	for i := 0; i+1 < len(pathVals); i += 2 {
		req.SetPathValue(pathVals[i], pathVals[i+1])
	}
	rec := httptest.NewRecorder()
	devjwt.Middleware(h).ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %d body %q: %v", rec.Code, rec.Body.String(), err)
	}
	return m
}

func TestAPIKeyLifecycle(t *testing.T) {
	s := newKeyServer(t)
	// create: plaintext returned once
	rec := keyReq(s.handleAPIKeyCreate, "POST", "/v1/apikeys", "tenant-a", map[string]string{"name": "erp-sync"})
	if rec.Code != 201 {
		t.Fatalf("create=%d %s", rec.Code, rec.Body)
	}
	created := decodeBody(t, rec)
	plain, _ := created["api_key"].(string)
	if !strings.HasPrefix(plain, "mrk_") {
		t.Fatalf("plaintext key missing/malformed: %v", created)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("no key id")
	}
	// list: metadata only — plaintext and hash never leave the store
	rec = keyReq(s.handleAPIKeyList, "GET", "/v1/apikeys", "tenant-a", nil)
	if rec.Code != 200 {
		t.Fatalf("list=%d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), plain) || strings.Contains(rec.Body.String(), "\"hash\"") {
		t.Fatalf("list leaks secret material: %s", rec.Body)
	}
	// rotate: new plaintext, old dies immediately
	rec = keyReq(s.handleAPIKeyRotate, "POST", "/v1/apikeys/"+id+"/rotate", "tenant-a", nil, "id", id)
	if rec.Code != 200 {
		t.Fatalf("rotate=%d %s", rec.Code, rec.Body)
	}
	rotated := decodeBody(t, rec)
	plain2, _ := rotated["api_key"].(string)
	if plain2 == "" || plain2 == plain {
		t.Fatal("rotation must return a fresh distinct secret")
	}
	if _, ok := s.apiKeys.Verify(plain); ok {
		t.Fatal("old secret still verifies after rotation")
	}
	if _, ok := s.apiKeys.Verify(plain2); !ok {
		t.Fatal("rotated secret does not verify")
	}
	// revoke
	rec = keyReq(s.handleAPIKeyRevoke, "POST", "/v1/apikeys/"+id+"/revoke", "tenant-a", nil, "id", id)
	if rec.Code != 200 {
		t.Fatalf("revoke=%d %s", rec.Code, rec.Body)
	}
	if _, ok := s.apiKeys.Verify(plain2); ok {
		t.Fatal("revoked secret still verifies")
	}
}

func TestAPIKeyStoreHashedOnly(t *testing.T) {
	dir := t.TempDir()
	ks, err := NewAPIKeyStore(dir + "/apikeys.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	k, plain, err := ks.Create("tenant-a", "ci-bot", "tester")
	if err != nil {
		t.Fatal(err)
	}
	// in-memory record holds hash + lookup prefix, not the secret
	if strings.Contains(k.Hash, plain) || k.Hash == "" || len(k.Hash) != 64 {
		t.Fatalf("hash field wrong: %q", k.Hash)
	}
	// durable file must contain neither the plaintext nor anything from
	// which it could be recovered (SHA-256 digest only)
	raw, err := os.ReadFile(dir + "/apikeys.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), plain) {
		t.Fatal("plaintext secret persisted to disk")
	}
	if !strings.Contains(string(raw), k.Hash) {
		t.Fatal("hash missing from durable record")
	}
	// reload from disk: verification still works (hash-only persistence)
	ks2, err := NewAPIKeyStore(dir + "/apikeys.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ks2.Verify(plain)
	if !ok || got.ID != k.ID || got.TenantID != "tenant-a" {
		t.Fatalf("verify after reload: ok=%v got=%+v", ok, got)
	}
	if _, ok := ks2.Verify(plain + "x"); ok {
		t.Fatal("wrong secret verified")
	}
}

func TestAPIKeyTenantIsolation(t *testing.T) {
	s := newKeyServer(t)
	rec := keyReq(s.handleAPIKeyCreate, "POST", "/v1/apikeys", "tenant-a", map[string]string{"name": "a-key"})
	if rec.Code != 201 {
		t.Fatalf("create=%d", rec.Code)
	}
	id, _ := decodeBody(t, rec)["id"].(string)
	// tenant-b list must not see tenant-a keys
	rec = keyReq(s.handleAPIKeyList, "GET", "/v1/apikeys", "tenant-b", nil)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), id) {
		t.Fatalf("cross-tenant list leak: %d %s", rec.Code, rec.Body)
	}
	// tenant-b cannot rotate/revoke tenant-a's key (404 — no oracle)
	for _, op := range []string{"rotate", "revoke"} {
		rec = keyReq(map[string]http.HandlerFunc{
			"rotate": s.handleAPIKeyRotate, "revoke": s.handleAPIKeyRevoke,
		}[op], "POST", "/v1/apikeys/"+id+"/"+op, "tenant-b", nil, "id", id)
		if rec.Code != 404 {
			t.Fatalf("cross-tenant %s=%d (want 404)", op, rec.Code)
		}
	}
	// role gating: auditor is forbidden; missing tenant claim is forbidden
	req := httptest.NewRequest("POST", "/v1/apikeys", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("X-Dev-Role", "auditor")
	req.Header.Set("X-Tenant-ID", "tenant-a")
	rec = httptest.NewRecorder()
	devjwt.Middleware(http.HandlerFunc(s.handleAPIKeyCreate)).ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("auditor create=%d (want 403)", rec.Code)
	}
	req = httptest.NewRequest("POST", "/v1/apikeys", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("X-Dev-Role", "admin") // no tenant claim
	rec = httptest.NewRecorder()
	devjwt.Middleware(http.HandlerFunc(s.handleAPIKeyCreate)).ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("tenant-less create=%d (want 403)", rec.Code)
	}
}

// TestAPIKeyAuthenticatesInvoiceCreate wires X-Api-Key through the
// middleware onto the invoice-create route; revoked keys get 401.
func TestAPIKeyAuthenticatesInvoiceCreate(t *testing.T) {
	s := newKeyServer(t)
	rec := keyReq(s.handleAPIKeyCreate, "POST", "/v1/apikeys", "tenant-a", map[string]string{"name": "erp"})
	if rec.Code != 201 {
		t.Fatalf("create=%d", rec.Code)
	}
	created := decodeBody(t, rec)
	plain, _ := created["api_key"].(string)
	id, _ := created["id"].(string)

	create := http.HandlerFunc(s.handleCreateInvoice)
	handler := s.apiKeyMiddleware(create, devjwt.Middleware(create))
	post := func(hdr string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(sampleInvoice())
		req := httptest.NewRequest("POST", "/v1/invoices", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		if hdr != "" {
			req.Header.Set("X-Api-Key", hdr)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	// valid key authenticates; invoice lands under the KEY's tenant
	rec = post(plain)
	if rec.Code != 201 {
		t.Fatalf("api-key invoice create=%d %s", rec.Code, rec.Body)
	}
	var resp struct {
		Invoices []*CanonicalInvoice `json:"invoices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Invoices) != 1 {
		t.Fatalf("decode: %v %s", err, rec.Body)
	}
	if resp.Invoices[0].TenantID != "tenant-a" {
		t.Fatalf("invoice tenant=%q (want tenant-a from key)", resp.Invoices[0].TenantID)
	}
	// garbage key: 401, no fallthrough
	if rec = post("mrk_deadbeef"); rec.Code != 401 {
		t.Fatalf("bad key=%d (want 401)", rec.Code)
	}
	// revoke -> 401
	if rec := keyReq(s.handleAPIKeyRevoke, "POST", "/v1/apikeys/"+id+"/revoke", "tenant-a", nil, "id", id); rec.Code != 200 {
		t.Fatalf("revoke=%d", rec.Code)
	}
	if rec = post(plain); rec.Code != 401 {
		t.Fatalf("revoked key=%d (want 401)", rec.Code)
	}
}
