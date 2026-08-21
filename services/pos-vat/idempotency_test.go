package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestIdempotentReplaySameKey (A1-01 regression): replaying a request with an
// identical Idempotency-Key MUST NOT ingest a second receipt. Pre-fix the
// replay returned 201 with a NEW receipt id (dedup lookup key never matched
// the stored random-ULID id); post-fix it must return 200 "duplicate" with
// the SAME receipt id.
func TestIdempotentReplaySameKey(t *testing.T) {
	svc := NewService(Config{DataDir: t.TempDir(), AuthMode: "dev", AttributionMode: "state"})
	svc.packs.LoadPacks()

	body := map[string]any{
		"idempotency_key": "IDEM-TEST-1",
		"merchant_tin":    "12345678-0001",
		"terminal_id":     "POS-9",
		"receipt_no":      "R-100",
		"lat":             6.5244, "lon": 3.3792,
		"lines": []map[string]any{{"sku": "A", "category": "electronics", "qty_milli": 1000, "unit_price_kobo": 50000}},
	}
	post := func() (int, map[string]any) {
		b, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/receipts", bytes.NewReader(b))
		svc.handleIngestReceipt(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code1, out1 := post()
	if code1 != 201 {
		t.Fatalf("first ingest: got %d (%v), want 201", code1, out1)
	}
	id1 := out1["receipt"].(map[string]any)["id"]

	code2, out2 := post()
	if code2 != 200 {
		t.Fatalf("replay: got %d (%v), want 200 duplicate", code2, out2)
	}
	if out2["status"] != "duplicate" {
		t.Fatalf("replay status = %v, want duplicate", out2["status"])
	}
	id2 := out2["receipt"].(map[string]any)["id"]
	if id1 != id2 {
		t.Fatalf("replay created a second receipt: %s vs %s", id1, id2)
	}
	if n := len(svc.store.ListReceipts("", "", 100)); n != 1 {
		t.Fatalf("store holds %d receipts, want exactly 1", n)
	}
}

// errCache is a cache that always fails — simulates a Redis outage (A1-13).
type errCache struct{}

func (errCache) SetNX(key, val string, ttl time.Duration) (bool, error) {
	return false, errors.New("redis down")
}
func (errCache) Get(key string) (string, error) { return "", errors.New("redis down") }

// TestIdempotencyCacheFailClosed (A1-13 regression): when the hot cache
// errors, the idempotency check must NOT be skipped (pre-fix fail-open);
// ingest must fail closed with 503, and the durable dedup must still catch
// replays.
func TestIdempotencyCacheFailClosed(t *testing.T) {
	svc := NewService(Config{DataDir: t.TempDir(), AuthMode: "dev", AttributionMode: "state"})
	svc.packs.LoadPacks()

	body := map[string]any{
		"idempotency_key": "IDEM-TEST-2",
		"merchant_tin":    "12345678-0001",
		"terminal_id":     "POS-9",
		"receipt_no":      "R-200",
		"lat":             6.5244, "lon": 3.3792,
		"lines": []map[string]any{{"sku": "A", "category": "electronics", "qty_milli": 1000, "unit_price_kobo": 50000}},
	}
	post := func() int {
		b, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		svc.handleIngestReceipt(rec, httptest.NewRequest("POST", "/v1/receipts", bytes.NewReader(b)))
		return rec.Code
	}

	if c := post(); c != http.StatusCreated {
		t.Fatalf("first ingest: got %d, want 201", c)
	}
	svc.cache = errCache{} // cache outage
	if c := post(); c != http.StatusOK {
		t.Fatalf("replay during cache outage: got %d, want 200 duplicate via durable store (fail-closed)", c)
	}
	// a NEW idempotency key during the outage must 503, not silently ingest
	body["idempotency_key"] = "IDEM-TEST-3"
	body["receipt_no"] = "R-201"
	if c := post(); c != http.StatusServiceUnavailable {
		t.Fatalf("new key during cache outage: got %d, want 503 fail-closed", c)
	}
	if n := len(svc.store.ListReceipts("", "", 100)); n != 1 {
		t.Fatalf("store holds %d receipts, want 1", n)
	}
}

// TestTINKeyProdGate (A1-02 regression): prod must refuse the missing/default
// TIN HMAC key; dev keeps the dev default.
func TestTINKeyProdGate(t *testing.T) {
	t.Setenv("PROFILE", "prod")
	t.Setenv("TIN_HMAC_KEY", "")
	if _, err := resolveTINKey(Config{AuthMode: "keycloak"}); err == nil {
		t.Fatal("prod without TIN_HMAC_KEY must fail closed")
	}
	t.Setenv("TIN_HMAC_KEY", "meridian-dev-tin-key")
	if _, err := resolveTINKey(Config{AuthMode: "keycloak"}); err == nil {
		t.Fatal("prod with the public dev TIN key must fail closed")
	}
	t.Setenv("TIN_HMAC_KEY", "s3cret-prod-key")
	k, err := resolveTINKey(Config{AuthMode: "keycloak"})
	if err != nil || k != "s3cret-prod-key" {
		t.Fatalf("prod with explicit key: err=%v key=%q", err, k)
	}
	t.Setenv("PROFILE", "dev")
	t.Setenv("TIN_HMAC_KEY", "")
	if k, err := resolveTINKey(Config{AuthMode: "dev"}); err != nil || k != "meridian-dev-tin-key" {
		t.Fatalf("dev fallback broken: err=%v key=%q", err, k)
	}
}
