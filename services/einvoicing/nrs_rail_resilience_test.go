package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// railCfg points a client at the test rail.
func railCfg(url string) LiveRailConfig {
	return LiveRailConfig{BaseURL: url, APIKey: "k", APISecret: "s", Email: "e@x.test", Password: "p"}
}

// Regression (R4 S3#5): a 2xx with an empty data.status must NOT be recorded
// as cleared/accepted — phantom clearance of a fiscally rejected invoice.
func TestLiveRailEmptyStatusIsNotCleared(t *testing.T) {
	for _, body := range []string{
		`{"data": {}}`,                                   // empty status
		`{"data": {"status": ""}}`,                       // explicit empty
		`{"data": {"status": "pending_review"}}`,         // unknown status
		`{"data": {"irn": "X-1", "status": "proxy-ok"}}`, // junk from a proxy
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/auth/login" {
				json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"access_token": "t"}})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		c := NewLiveRailClient(railCfg(srv.URL))
		_, err := c.Preclear(context.Background(), &CanonicalInvoice{InvoiceNumber: "INV-1", IRN: "INV-1-ABCDEFGH-20260101"}, []byte("<xml/>"))
		if err == nil {
			t.Errorf("Preclear with body %s: want fail-closed error, got clearance", body)
		}
		_, err = c.ReportB2C(context.Background(), &CanonicalInvoice{InvoiceNumber: "INV-1"})
		if err == nil {
			t.Errorf("ReportB2C with body %s: want fail-closed error, got acceptance", body)
		}
		srv.Close()
	}
}

// Known statuses still map through.
func TestLiveRailExplicitStatusesPass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/login" {
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"access_token": "t"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"irn": "INV-1-ABCDEFGH-20260101", "status": "rejected"},
		})
	}))
	defer srv.Close()
	c := NewLiveRailClient(railCfg(srv.URL))
	res, err := c.Preclear(context.Background(), &CanonicalInvoice{InvoiceNumber: "INV-1"}, []byte("<xml/>"))
	if err != nil || res.Status != "rejected" {
		t.Fatalf("want explicit rejection, got %+v err=%v", res, err)
	}
}

// Regression (R4 S3#6): a 401 invalidates the cached JWT, triggers ONE
// re-login (single-flighted), and retries the request once with the fresh
// token — the rail self-heals instead of failing permanently.
func TestLiveRail401RefreshesTokenAndRetries(t *testing.T) {
	var logins, uploads int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			n := atomic.AddInt64(&logins, 1)
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"access_token": fmt.Sprintf("jwt-%d", n)}})
		case "/api/v1/invoice/upload":
			atomic.AddInt64(&uploads, 1)
			if r.Header.Get("Authorization") == "Bearer jwt-1" {
				w.WriteHeader(http.StatusUnauthorized) // simulate expiry
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"irn": "INV-1-ABCDEFGH-20260101", "status": "cleared"},
			})
		}
	}))
	defer srv.Close()
	c := NewLiveRailClient(railCfg(srv.URL))
	res, err := c.Preclear(context.Background(), &CanonicalInvoice{InvoiceNumber: "INV-1"}, []byte("<xml/>"))
	if err != nil {
		t.Fatalf("Preclear after 401 refresh: %v", err)
	}
	if res.Status != "cleared" {
		t.Fatalf("result = %+v", res)
	}
	if atomic.LoadInt64(&logins) != 2 || atomic.LoadInt64(&uploads) != 2 {
		t.Fatalf("logins=%d uploads=%d, want 2/2", logins, uploads)
	}
}

// Concurrent first-use runs exactly one login (no stampede).
func TestLiveRailConcurrentLoginSingleflight(t *testing.T) {
	var logins int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/login" {
			atomic.AddInt64(&logins, 1)
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"access_token": "t"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"irn": "I", "status": "accepted"}})
	}))
	defer srv.Close()
	c := NewLiveRailClient(railCfg(srv.URL))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.ReportB2C(context.Background(), &CanonicalInvoice{InvoiceNumber: "INV"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := atomic.LoadInt64(&logins); n != 1 {
		t.Fatalf("logins = %d, want exactly 1 (singleflight)", n)
	}
}

// Regression (R4 S3#14): the live profile refuses plaintext http and refuses
// silent fallback from MBS_LIVE_* to legacy NRS_* variables.
func TestLiveProfileRequiresHTTPSAndMBSLiveVars(t *testing.T) {
	// http:// base URL is refused.
	t.Setenv("MBS_LIVE_BASE_URL", "http://api.einvoice.firs.gov.ng")
	t.Setenv("MBS_LIVE_API_KEY", "k")
	t.Setenv("MBS_LIVE_API_SECRET", "s")
	t.Setenv("MBS_LIVE_EMAIL", "e@x.test")
	t.Setenv("MBS_LIVE_PASSWORD", "p")
	if _, err := LiveRailConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("http base URL: want https refusal, got %v", err)
	}

	// NRS_* only (no MBS_LIVE_*): previously accepted via fallback, now refused.
	t.Setenv("MBS_LIVE_BASE_URL", "")
	t.Setenv("MBS_LIVE_API_KEY", "")
	t.Setenv("MBS_LIVE_API_SECRET", "")
	t.Setenv("MBS_LIVE_EMAIL", "")
	t.Setenv("MBS_LIVE_PASSWORD", "")
	t.Setenv("NRS_BASE_URL", "https://api.einvoice.firs.gov.ng")
	t.Setenv("NRS_API_KEY", "k")
	t.Setenv("NRS_API_SECRET", "s")
	t.Setenv("NRS_EMAIL", "e@x.test")
	t.Setenv("NRS_PASSWORD", "p")
	if _, err := LiveRailConfigFromEnv(); err == nil {
		t.Fatal("NRS_*-only config: want fail-closed refusal of silent fallback")
	}
}
