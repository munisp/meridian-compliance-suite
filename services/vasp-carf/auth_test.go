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
	"testing"
	"time"

	"github.com/munisp/meridian-compliance-suite/packages/authx"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwksServer serves a JWKS document for priv and signs test tokens.
type jwksServer struct {
	priv *rsa.PrivateKey
	srv  *httptest.Server
}

func newJWKS(t *testing.T) *jwksServer {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	j := &jwksServer{priv: priv}
	j.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(priv.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.PublicKey.E)).Bytes())
		w.Write([]byte(`{"keys":[{"kty":"RSA","kid":"k1","alg":"RS256","use":"sig","n":"` + n + `","e":"` + e + `"}]}`))
	}))
	t.Cleanup(j.srv.Close)
	return j
}

func (j *jwksServer) token(t *testing.T, issuer string, roles ...string) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	pl, _ := json.Marshal(map[string]any{
		"sub": "svc-account", "iss": issuer,
		"exp":          time.Now().Add(time.Hour).Unix(),
		"iat":          time.Now().Unix(),
		"realm_access": map[string]any{"roles": roles},
	})
	msg := b64(hdr) + "." + b64(pl)
	sig, err := signRS256(j.priv, []byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	return msg + "." + b64(sig)
}

func signRS256(priv *rsa.PrivateKey, msg []byte) ([]byte, error) {
	h := sha256.Sum256(msg)
	return rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, h[:])
}

func kcService(issuer, jwksURL string) *Service {
	return &Service{
		cfg: Config{AuthMode: "keycloak", JWTSecret: "meridian-dev-secret"},
		kc:  authx.NewKeycloakVerifier(issuer, "", jwksURL),
	}
}

func okHandler(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }

func TestKeycloakAcceptsValidRS256(t *testing.T) {
	j := newJWKS(t)
	svc := kcService(j.srv.URL, j.srv.URL)
	req := httptest.NewRequest("GET", "/v1/packs", nil)
	req.Header.Set("Authorization", "Bearer "+j.token(t, j.srv.URL, "operator"))
	rec := httptest.NewRecorder()
	svc.auth(okHandler)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestKeycloakRejectsForgedHS256DevToken(t *testing.T) {
	j := newJWKS(t)
	svc := kcService(j.srv.URL, j.srv.URL)
	// forge an HS256 token with the public default dev secret
	forged, err := authx.IssueDev("meridian-dev-secret", authx.Claims{Sub: "attacker", Roles: []string{"admin"}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v1/packs", nil)
	req.Header.Set("Authorization", "Bearer "+forged)
	rec := httptest.NewRecorder()
	svc.auth(okHandler)(rec, req)
	if rec.Code != 401 {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestKeycloakRejectsWrongIssuer(t *testing.T) {
	j := newJWKS(t)
	svc := kcService(j.srv.URL, j.srv.URL)
	req := httptest.NewRequest("GET", "/v1/packs", nil)
	req.Header.Set("Authorization", "Bearer "+j.token(t, "https://evil.example", "operator"))
	rec := httptest.NewRecorder()
	svc.auth(okHandler)(rec, req)
	if rec.Code != 401 {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestKeycloakIgnoresDevRoleHeader(t *testing.T) {
	j := newJWKS(t)
	svc := kcService(j.srv.URL, j.srv.URL)
	req := httptest.NewRequest("GET", "/v1/packs", nil)
	req.Header.Set("X-Dev-Role", "admin")
	rec := httptest.NewRecorder()
	svc.auth(okHandler)(rec, req)
	if rec.Code != 401 {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestDevModeAllowlistAndHS256(t *testing.T) {
	svc := &Service{cfg: Config{AuthMode: "dev", JWTSecret: "meridian-dev-secret"}}
	// arbitrary role header is rejected
	req := httptest.NewRequest("GET", "/v1/packs", nil)
	req.Header.Set("X-Dev-Role", "root")
	rec := httptest.NewRecorder()
	svc.auth(okHandler)(rec, req)
	if rec.Code != 401 {
		t.Fatalf("want 401 for non-allowlisted role, got %d", rec.Code)
	}
	// allowlisted role works
	req = httptest.NewRequest("GET", "/v1/packs", nil)
	req.Header.Set("X-Dev-Role", "operator")
	rec = httptest.NewRecorder()
	svc.auth(okHandler)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	// HS256 dev token works in dev mode
	tok, _ := authx.IssueDev("meridian-dev-secret", authx.Claims{Sub: "u", Roles: []string{"operator"}})
	req = httptest.NewRequest("GET", "/v1/packs", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	svc.auth(okHandler)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("want 200, got %d", rec.Code)
	}
}
