package main

// R3 verifier regression: a VALID HS256 JWT with NO roles claim plus a forged
// inbound `X-Role: admin` header previously bypassed the admin gate on
// /v1/relations/grant, because main.go only overwrote X-Role when the JWT
// carried roles. Fix: auth() unconditionally strips inbound identity headers
// (X-Role, X-Subject, X-Meridian-*) before resolving identity from the token.

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/munisp/meridian-compliance-suite/packages/authx"
)

func TestForgedXRoleHeaderWithRolelessJWTDenied(t *testing.T) {
	svc := newTestService(t)
	// valid JWT, no roles claim
	tok, err := authx.IssueDev(svc.cfg.JWTSecret, authx.Claims{Sub: "mallam-fraud"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(RelationTuple{Entity: "matter:mtr-9", Relation: "counsel", Subject: "user:mallam-fraud"})
	req := httptest.NewRequest("POST", "/v1/relations/grant", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Role", "admin") // forged inbound identity header
	rec := httptest.NewRecorder()
	svc.auth(svc.handleRelGrant)(rec, req)
	if rec.Code != 403 {
		t.Fatalf("roleless JWT + forged X-Role grant got %d, want 403: %s", rec.Code, rec.Body)
	}
	if svc.rel.Check("matter:mtr-9", "write", "user:mallam-fraud", svc.store) {
		t.Fatal("forged self-grant took effect despite 403")
	}
}

func TestJWTRoleStillGrantsAdmin(t *testing.T) {
	svc := newTestService(t)
	tok, err := authx.IssueDev(svc.cfg.JWTSecret, authx.Claims{Sub: "boss", Roles: []string{"admin"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(RelationTuple{Entity: "matter:mtr-9", Relation: "counsel", Subject: "user:lawyer-1"})
	req := httptest.NewRequest("POST", "/v1/relations/grant", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	svc.auth(svc.handleRelGrant)(rec, req)
	if rec.Code != 201 {
		t.Fatalf("admin JWT grant got %d, want 201: %s", rec.Code, rec.Body)
	}
}
