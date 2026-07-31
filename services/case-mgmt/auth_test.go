package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/munisp/meridian-compliance-suite/packages/authx"
)

func okHandler(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }

// TestKeycloakModeIgnoresDevRoleHeader (audit fix H-1): when the keycloak
// verifier is active, the forgeable X-Dev-Role header must not authenticate.
func TestKeycloakModeIgnoresDevRoleHeader(t *testing.T) {
	svc := newTestService(t)
	svc.cfg.AuthMode = "keycloak"
	svc.kc = authx.NewKeycloakVerifier("https://keycloak:8443/realms/meridian", "", "http://127.0.0.1:1/jwks")
	req := httptest.NewRequest("GET", "/v1/matters", nil)
	req.Header.Set("X-Dev-Role", "admin")
	rec := httptest.NewRecorder()
	svc.auth(okHandler)(rec, req)
	if rec.Code != 401 {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

// TestKeycloakModeRejectsForgedHS256: an HS256 token signed with the public
// dev secret must be rejected when the keycloak verifier is active.
func TestKeycloakModeRejectsForgedHS256(t *testing.T) {
	svc := newTestService(t)
	svc.cfg.AuthMode = "keycloak"
	svc.kc = authx.NewKeycloakVerifier("https://keycloak:8443/realms/meridian", "", "http://127.0.0.1:1/jwks")
	forged, err := authx.IssueDev("meridian-dev-secret", authx.Claims{Sub: "attacker", Roles: []string{"admin"}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v1/matters", nil)
	req.Header.Set("Authorization", "Bearer "+forged)
	rec := httptest.NewRecorder()
	svc.auth(okHandler)(rec, req)
	if rec.Code != 401 {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

// TestDevModeStillAcceptsDevRole: dev mode is unchanged.
func TestDevModeStillAcceptsDevRole(t *testing.T) {
	svc := newTestService(t)
	req := httptest.NewRequest("GET", "/v1/matters", nil)
	req.Header.Set("X-Dev-Role", "operator")
	rec := httptest.NewRecorder()
	svc.auth(okHandler)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("want 200, got %d", rec.Code)
	}
}
