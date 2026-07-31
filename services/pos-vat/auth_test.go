package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/munisp/meridian-compliance-suite/packages/authx"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func rs256Token(t *testing.T, priv *rsa.PrivateKey, issuer string, roles ...string) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	pl, _ := json.Marshal(map[string]any{
		"sub": "svc-account", "iss": issuer,
		"exp":          time.Now().Add(time.Hour).Unix(),
		"iat":          time.Now().Unix(),
		"realm_access": map[string]any{"roles": roles},
	})
	msg := b64(hdr) + "." + b64(pl)
	h := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return msg + "." + b64(sig)
}

func okHandler(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }

// TestKeycloakAuth exercises SPEC §1.3 keycloak mode: a live JWKS test
// server, acceptance of a valid RS256 token, and rejection of a forged
// HS256 token signed with the public dev secret.
func TestKeycloakAuth(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(priv.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.PublicKey.E)).Bytes())
		w.Write([]byte(`{"keys":[{"kty":"RSA","kid":"k1","alg":"RS256","use":"sig","n":"` + n + `","e":"` + e + `"}]}`))
	}))
	defer jwks.Close()

	t.Setenv("KEYCLOAK_ISSUER", jwks.URL)
	t.Setenv("KEYCLOAK_JWKS_URL", jwks.URL)
	os.Unsetenv("KEYCLOAK_AUDIENCE")

	svc := &Service{cfg: Config{AuthMode: "keycloak", JWTSecret: "meridian-dev-secret"}}
	handler := svc.auth(okHandler) // registers + builds the verifier (fail-closed path)

	// valid RS256 token -> 200
	req := httptest.NewRequest("GET", "/v1/packs", nil)
	req.Header.Set("Authorization", "Bearer "+rs256Token(t, priv, jwks.URL, "operator"))
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != 200 {
		t.Fatalf("valid RS256: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	// forged HS256 token with the public dev secret -> 401
	forged, err := authx.IssueDev("meridian-dev-secret", authx.Claims{Sub: "attacker", Roles: []string{"admin"}})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("GET", "/v1/packs", nil)
	req.Header.Set("Authorization", "Bearer "+forged)
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != 401 {
		t.Fatalf("forged HS256: want 401, got %d", rec.Code)
	}

	// X-Dev-Role header is ignored in keycloak mode -> 401
	req = httptest.NewRequest("GET", "/v1/packs", nil)
	req.Header.Set("X-Dev-Role", "admin")
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != 401 {
		t.Fatalf("X-Dev-Role in keycloak mode: want 401, got %d", rec.Code)
	}
}

// TestDevModeRoleAllowlist: non-allowlisted X-Dev-Role values are rejected.
func TestDevModeRoleAllowlist(t *testing.T) {
	svc := &Service{cfg: Config{AuthMode: "dev", JWTSecret: "meridian-dev-secret"}}
	handler := svc.auth(okHandler)
	req := httptest.NewRequest("GET", "/v1/packs", nil)
	req.Header.Set("X-Dev-Role", "root")
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != 401 {
		t.Fatalf("want 401 for non-allowlisted role, got %d", rec.Code)
	}
}
