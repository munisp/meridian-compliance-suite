// permify_test.go — P0 authz: live client fake-transport, dev fallback,
// prod fail-closed, and schema consistency (every permission string checked
// in code must exist in schemas/case-mgmt.perm, vendored from the canonical
// core permify-models schema family).
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sync/atomic"
	"testing"
	"time"
)

func newTestPermifyClient(h http.HandlerFunc) (*PermifyClient, func()) {
	srv := httptest.NewServer(h)
	return NewPermifyClient(srv.URL, "t1"), srv.Close
}

func TestPermifyCheckAllowedDenied(t *testing.T) {
	c, done := newTestPermifyClient(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Subject struct{ ID string } `json:"subject"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Subject.ID == "u1" {
			w.Write([]byte(`{"can":"RESULT_ALLOWED"}`))
		} else {
			w.Write([]byte(`{"can":"RESULT_DENIED"}`))
		}
	})
	defer done()
	ok, err := c.Check(context.Background(), "matter:m1", "read", "user:u1")
	if err != nil || !ok {
		t.Fatalf("want allowed, got %v %v", ok, err)
	}
	ok, err = c.Check(context.Background(), "matter:m1", "read", "user:u2")
	if err != nil || ok {
		t.Fatalf("want denied nil-error, got %v %v", ok, err)
	}
}

func TestPermifyCheckRetriesOn5xx(t *testing.T) {
	var calls int32
	c, done := newTestPermifyClient(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"can":"RESULT_ALLOWED"}`))
	})
	defer done()
	ok, err := c.Check(context.Background(), "doc:d1", "read", "user:u1")
	if err != nil || !ok {
		t.Fatalf("want allowed after retry, got %v %v", ok, err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("want exactly one retry, got %d calls", calls)
	}
}

func TestPermifyCheckTimeout(t *testing.T) {
	c, done := newTestPermifyClient(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte(`{"can":"RESULT_ALLOWED"}`))
	})
	defer done()
	c.timeout = 50 * time.Millisecond
	ok, err := c.Check(context.Background(), "matter:m1", "write", "user:u1")
	if err == nil || ok {
		t.Fatalf("want timeout error+denied, got %v %v", ok, err)
	}
}

func TestWirePermifyDevFallback(t *testing.T) {
	t.Setenv("PERMIFY_URL", "")
	t.Setenv("PROFILE", "")
	c, err := wirePermify("dev")
	if err != nil || c != nil {
		t.Fatalf("dev without PERMIFY_URL must fall back, got %v %v", c, err)
	}
}

func TestWirePermifyProdFailClosed(t *testing.T) {
	t.Setenv("PERMIFY_URL", "")
	t.Setenv("PROFILE", "prod")
	if _, err := wirePermify("dev"); err == nil {
		t.Fatal("PROFILE=prod without PERMIFY_URL must fail closed")
	}
	t.Setenv("PROFILE", "")
	if _, err := wirePermify("keycloak"); err == nil {
		t.Fatal("AUTH_MODE=keycloak without PERMIFY_URL must fail closed")
	}
}

func TestWirePermifyLive(t *testing.T) {
	t.Setenv("PERMIFY_URL", "http://permify:3476")
	c, err := wirePermify("keycloak")
	if err != nil || c == nil {
		t.Fatalf("PERMIFY_URL set must yield live client, got %v %v", c, err)
	}
}

// TestCheckRelLiveFailClosed: Permify unreachable -> deny.
func TestCheckRelLiveFailClosed(t *testing.T) {
	svc := &Service{perm: NewPermifyClient("http://127.0.0.1:1", "t1")}
	req := httptest.NewRequest("GET", "/", nil)
	if svc.checkRel(req, "matter:m1", "read", "user:u1") {
		t.Fatal("unreachable Permify must fail closed")
	}
	if svc.relCheckerMode() != "permify-live" {
		t.Fatal("mode label must be permify-live when wired")
	}
}

// TestSchemaConsistency: every permission string used by the checker code
// paths must be declared in schemas/case-mgmt.perm.
func TestSchemaConsistency(t *testing.T) {
	b, err := os.ReadFile("schemas/case-mgmt.perm")
	if err != nil {
		t.Fatalf("schema file missing: %v", err)
	}
	re := regexp.MustCompile(`(?m)^\s*permission\s+([a-z_]+)\s*=`)
	declared := map[string]bool{}
	for _, m := range re.FindAllSubmatch(b, -1) {
		declared[string(m[1])] = true
	}
	// permissions checked by handlers.go / store.go call sites
	for _, p := range []string{"read", "write", "privileged", "share"} {
		if !declared[p] {
			t.Errorf("permission %q checked in code but missing from schemas/case-mgmt.perm", p)
		}
	}
}
