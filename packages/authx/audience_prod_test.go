package authx

import "testing"

// TestProdKeycloakRequiresAudience (A1-08 regression): PROFILE=prod +
// AUTH_MODE=keycloak without KEYCLOAK_AUDIENCE must be a startup error
// (fail-closed). Pre-fix the config resolved with an empty audience and any
// realm token was accepted.
func TestProdKeycloakRequiresAudience(t *testing.T) {
	t.Setenv("PROFILE", "prod")
	t.Setenv("AUTH_MODE", "keycloak")
	t.Setenv("KEYCLOAK_ISSUER", "https://kc.example/realms/meridian")
	t.Setenv("KEYCLOAK_AUDIENCE", "")
	if _, _, err := middlewareConfigFromEnv(); err == nil {
		t.Fatal("prod keycloak without KEYCLOAK_AUDIENCE must fail closed")
	}
	t.Setenv("KEYCLOAK_AUDIENCE", "nrs-api")
	mode, kc, err := middlewareConfigFromEnv()
	if err != nil {
		t.Fatalf("prod keycloak with audience: %v", err)
	}
	if mode != "keycloak" || kc == nil || kc.Audience != "nrs-api" {
		t.Fatalf("bad config: mode=%s kc=%+v", mode, kc)
	}
	// non-prod keycloak remains audience-optional (dev convenience)
	t.Setenv("PROFILE", "dev")
	t.Setenv("KEYCLOAK_AUDIENCE", "")
	if _, _, err := middlewareConfigFromEnv(); err != nil {
		t.Fatalf("non-prod keycloak without audience must still work: %v", err)
	}
}
