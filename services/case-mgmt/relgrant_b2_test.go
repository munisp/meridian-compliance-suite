package main

// B2 regression (finding #3): relation grant/revoke previously required
// only authentication — any authenticated principal could self-grant
// Permify relations (e.g. matter counsel), defeating every checkRel gate.
//
// Post-fix: grant/revoke require the admin role, the relation must be in
// the schema-allowed set, and the acting principal is the verified JWT
// subject (recorded in the audit log).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func relReq(t *testing.T, svc *Service, method, path, role, subj string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if role != "" {
		req.Header.Set("X-Dev-Role", role)
	}
	if subj != "" {
		req.Header.Set("X-Dev-Subject", subj)
	}
	rec := httptest.NewRecorder()
	h := svc.auth(map[string]http.HandlerFunc{
		"grant":  svc.handleRelGrant,
		"revoke": svc.handleRelRevoke,
	}[path[len("/v1/relations/"):]])
	h(rec, req)
	return rec
}

func TestRelGrantDeniedForNonAdmin(t *testing.T) {
	svc := newTestService(t)
	// operator self-grants counsel on a matter -> must be 403
	rec := relReq(t, svc, "POST", "/v1/relations/grant", "operator", "mallam-fraud",
		RelationTuple{Entity: "matter:mtr-9", Relation: "counsel", Subject: "user:mallam-fraud"})
	if rec.Code != 403 {
		t.Fatalf("non-admin self-grant got %d, want 403: %s", rec.Code, rec.Body)
	}
	if svc.rel.Check("matter:mtr-9", "write", "user:mallam-fraud", svc.store) {
		t.Fatal("self-grant took effect despite 403")
	}
}

func TestRelGrantAnonymousDenied(t *testing.T) {
	svc := newTestService(t)
	rec := relReq(t, svc, "POST", "/v1/relations/grant", "", "",
		RelationTuple{Entity: "matter:mtr-9", Relation: "counsel", Subject: "user:x"})
	if rec.Code != 401 {
		t.Fatalf("anonymous grant got %d, want 401", rec.Code)
	}
}

func TestRelGrantAdminAllowedRelation(t *testing.T) {
	svc := newTestService(t)
	rec := relReq(t, svc, "POST", "/v1/relations/grant", "admin", "boss",
		RelationTuple{Entity: "matter:mtr-9", Relation: "counsel", Subject: "user:lawyer-1"})
	if rec.Code != 201 {
		t.Fatalf("admin grant got %d, want 201: %s", rec.Code, rec.Body)
	}
	if !svc.rel.Check("matter:mtr-9", "write", "user:lawyer-1", svc.store) {
		t.Fatal("admin grant did not take effect")
	}
	// revoke also admin-only
	rec = relReq(t, svc, "POST", "/v1/relations/revoke", "operator", "mallam-fraud",
		RelationTuple{Entity: "matter:mtr-9", Relation: "counsel", Subject: "user:lawyer-1"})
	if rec.Code != 403 {
		t.Fatalf("non-admin revoke got %d, want 403", rec.Code)
	}
	rec = relReq(t, svc, "POST", "/v1/relations/revoke", "admin", "boss",
		RelationTuple{Entity: "matter:mtr-9", Relation: "counsel", Subject: "user:lawyer-1"})
	if rec.Code != 200 {
		t.Fatalf("admin revoke got %d, want 200: %s", rec.Code, rec.Body)
	}
	if svc.rel.Check("matter:mtr-9", "write", "user:lawyer-1", svc.store) {
		t.Fatal("revoke did not take effect")
	}
}

func TestRelGrantRejectsRelationOutsideAllowedSet(t *testing.T) {
	svc := newTestService(t)
	for _, rel := range []string{"admin", "requeue", "owner_of_everything"} {
		rec := relReq(t, svc, "POST", "/v1/relations/grant", "admin", "boss",
			RelationTuple{Entity: "matter:mtr-9", Relation: rel, Subject: "user:x"})
		if rec.Code != 400 {
			t.Fatalf("grant of unknown relation %q got %d, want 400", rel, rec.Code)
		}
	}
	// entity type/relation mismatch: doc relation on a matter entity
	rec := relReq(t, svc, "POST", "/v1/relations/grant", "admin", "boss",
		RelationTuple{Entity: "matter:mtr-9", Relation: "owner", Subject: "user:x"})
	if rec.Code != 400 {
		t.Fatalf("matter:owner grant got %d, want 400", rec.Code)
	}
	// malformed refs
	rec = relReq(t, svc, "POST", "/v1/relations/grant", "admin", "boss",
		RelationTuple{Entity: "mtr-9", Relation: "counsel", Subject: "x"})
	if rec.Code != 400 {
		t.Fatalf("unscoped refs grant got %d, want 400", rec.Code)
	}
}
