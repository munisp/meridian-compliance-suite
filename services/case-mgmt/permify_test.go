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
	"strings"
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

// fakePermify is an in-memory Permify Data API double: it honors
// relationships write/delete/read and answers permission checks from the
// tuple set (relation == permission, subject match).
type fakePermify struct {
	tuples map[string]bool // entity|relation|subject
}

func newFakePermify() (*PermifyClient, func()) {
	f := &fakePermify{tuples: map[string]bool{}}
	type ref struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tenants/t1/data/relationships/write", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Tuple struct {
				Entity   ref    `json:"entity"`
				Relation string `json:"relation"`
				Subject  ref    `json:"subject"`
			} `json:"tuple"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.tuples[b.Tuple.Entity.Type+":"+b.Tuple.Entity.ID+"|"+b.Tuple.Relation+"|"+b.Tuple.Subject.Type+":"+b.Tuple.Subject.ID] = true
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"snap_token":"tok"}`))
	})
	mux.HandleFunc("POST /v1/tenants/t1/data/relationships/delete", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Filter struct {
				Entity   ref    `json:"entity"`
				Relation string `json:"relation"`
				Subject  ref    `json:"subject"`
			} `json:"filter"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		delete(f.tuples, b.Filter.Entity.Type+":"+b.Filter.Entity.ID+"|"+b.Filter.Relation+"|"+b.Filter.Subject.Type+":"+b.Filter.Subject.ID)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"snap_token":"tok"}`))
	})
	mux.HandleFunc("POST /v1/tenants/t1/data/relationships/read", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{"tuples": []map[string]any{}}
		for k := range f.tuples {
			var e, s string
			var rel string
			parts := split3(k)
			e, rel, s = parts[0], parts[1], parts[2]
			out["tuples"] = append(out["tuples"].([]map[string]any), map[string]any{
				"entity":   map[string]string{"type": split2(e)[0], "id": split2(e)[1]},
				"relation": rel,
				"subject":  map[string]string{"type": split2(s)[0], "id": split2(s)[1]},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /v1/tenants/t1/permissions/check", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Entity     ref    `json:"entity"`
			Permission string `json:"permission"`
			Subject    ref    `json:"subject"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		allowed := f.tuples[b.Entity.Type+":"+b.Entity.ID+"|"+b.Permission+"|"+b.Subject.Type+":"+b.Subject.ID]
		can := "RESULT_DENIED"
		if allowed {
			can = "RESULT_ALLOWED"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"can": can})
	})
	srv := httptest.NewServer(mux)
	return NewPermifyClient(srv.URL, "t1"), srv.Close
}

func split2(s string) []string {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return []string{s[:i], s[i+1:]}
		}
	}
	return []string{s, ""}
}

func split3(s string) []string {
	out := []string{}
	cur := ""
	for i := 0; i < len(s); i++ {
		if s[i] == '|' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(s[i])
	}
	return append(out, cur)
}

// TestPermifyGrantCheckRevokeCheck proves the live write path end to end:
// grant -> check true -> revoke -> check false, plus list reflects writes.
func TestPermifyGrantCheckRevokeCheck(t *testing.T) {
	c, closeFn := newFakePermify()
	defer closeFn()
	ctx := context.Background()

	allowed, err := c.Check(ctx, "matter:m1", "counsel", "user:u1")
	if err != nil || allowed {
		t.Fatalf("pre-grant check must be false: allowed=%v err=%v", allowed, err)
	}
	if err := c.WriteRelationship(ctx, "matter:m1", "counsel", "user:u1"); err != nil {
		t.Fatalf("write: %v", err)
	}
	allowed, err = c.Check(ctx, "matter:m1", "counsel", "user:u1")
	if err != nil || !allowed {
		t.Fatalf("post-grant check must be true: allowed=%v err=%v", allowed, err)
	}
	tuples, err := c.ReadRelationships(ctx, "", "")
	if err != nil || len(tuples) != 1 {
		t.Fatalf("list after grant: %+v err=%v", tuples, err)
	}
	if tuples[0].Entity != "matter:m1" || tuples[0].Relation != "counsel" || tuples[0].Subject != "user:u1" {
		t.Fatalf("unexpected tuple: %+v", tuples[0])
	}
	if err := c.DeleteRelationship(ctx, "matter:m1", "counsel", "user:u1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	allowed, err = c.Check(ctx, "matter:m1", "counsel", "user:u1")
	if err != nil || allowed {
		t.Fatalf("post-revoke check must be false: allowed=%v err=%v", allowed, err)
	}
	tuples, err = c.ReadRelationships(ctx, "", "")
	if err != nil || len(tuples) != 0 {
		t.Fatalf("list after revoke: %+v err=%v", tuples, err)
	}
}

// TestRelGrantRevokeHandlersLive exercises the HTTP handlers against the
// fake Permify server (live mode): grant 201, revoke 200, list 200.
func TestRelGrantRevokeHandlersLive(t *testing.T) {
	c, closeFn := newFakePermify()
	defer closeFn()
	svc := &Service{perm: c}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/relations/grant", svc.handleRelGrant)
	mux.HandleFunc("POST /v1/relations/revoke", svc.handleRelRevoke)
	mux.HandleFunc("GET /v1/relations", svc.handleRelList)

	post := func(path string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	tuple := `{"entity":"matter:m9","relation":"counsel","subject":"user:u9"}`
	if rec := post("/v1/relations/grant", tuple); rec.Code != 201 {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body)
	}
	allowed, err := c.Check(context.Background(), "matter:m9", "counsel", "user:u9")
	if err != nil || !allowed {
		t.Fatalf("grant must be visible to checks: allowed=%v err=%v", allowed, err)
	}
	req := httptest.NewRequest("GET", "/v1/relations", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "matter:m9") {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := post("/v1/relations/revoke", tuple); rec.Code != 200 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	allowed, err = c.Check(context.Background(), "matter:m9", "counsel", "user:u9")
	if err != nil || allowed {
		t.Fatalf("revoke must be visible to checks: allowed=%v err=%v", allowed, err)
	}
}
