package authx

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	pl, _ := json.Marshal(claims)
	msg := b64(hdr) + "." + b64(pl)
	digest := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return msg + "." + b64(sig)
}

func jwksServer(t *testing.T, pub *rsa.PublicKey, kid string) *httptest.Server {
	t.Helper()
	e := pub.E
	eb := []byte{}
	for e > 0 {
		eb = append([]byte{byte(e)}, eb...)
		e >>= 8
	}
	doc := map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
		"n": b64(pub.N.Bytes()), "e": b64(eb),
	}}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(doc)
	}))
}

func TestKeycloakVerify(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	kid := "test-key-1"
	srv := jwksServer(t, &key.PublicKey, kid)
	defer srv.Close()

	issuer := "https://keycloak:8443/realms/meridian"
	v := NewKeycloakVerifier(issuer, "meridian-services", srv.URL)

	tok := signRS256(t, key, kid, map[string]any{
		"sub": "user-1", "iss": issuer, "aud": "meridian-services",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"tenant_id":       "t-1",
		"realm_access":    map[string]any{"roles": []string{"admin", "operator"}},
		"resource_access": map[string]any{"meridian-services": map[string]any{"roles": []string{"svc"}}},
	})
	c, err := v.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if c.Sub != "user-1" || c.TenantID != "t-1" {
		t.Fatalf("claims: %+v", c)
	}
	want := map[string]bool{"admin": true, "operator": true, "svc": true}
	for _, r := range c.Roles {
		delete(want, r)
	}
	if len(want) != 0 {
		t.Fatalf("missing roles %v in %v", want, c.Roles)
	}

	// wrong audience
	badAud := signRS256(t, key, kid, map[string]any{
		"sub": "u", "iss": issuer, "aud": "other", "exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := v.Verify(badAud); err == nil || !strings.Contains(err.Error(), "audience") {
		t.Fatalf("want audience error, got %v", err)
	}
	// wrong issuer
	badIss := signRS256(t, key, kid, map[string]any{
		"sub": "u", "iss": "https://evil", "aud": "meridian-services", "exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := v.Verify(badIss); err == nil {
		t.Fatal("want issuer error")
	}
	// expired
	expired := signRS256(t, key, kid, map[string]any{
		"sub": "u", "iss": issuer, "aud": "meridian-services", "exp": time.Now().Add(-time.Hour).Unix(),
	})
	if _, err := v.Verify(expired); err == nil {
		t.Fatal("want expiry error")
	}
	// unknown kid (key rotation -> refresh still fails to find it)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	rot := signRS256(t, other, "unknown-kid", map[string]any{
		"sub": "u", "iss": issuer, "aud": "meridian-services", "exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := v.Verify(rot); err == nil {
		t.Fatal("want unknown kid error")
	}
	// HS256 alg confusion rejected
	hs, _ := IssueDev("x", Claims{Sub: "u", Exp: time.Now().Add(time.Hour).Unix()})
	if _, err := v.Verify(hs); err == nil {
		t.Fatal("want alg rejection")
	}
	_ = fmt.Sprint()
}

func TestDevRoundTrip(t *testing.T) {
	tok, err := IssueDev("s3cret", Claims{Sub: "u1", Roles: []string{"admin"}, TenantID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := VerifyDev("s3cret", tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.Sub != "u1" || c.Roles[0] != "admin" {
		t.Fatalf("%+v", c)
	}
	if _, err := VerifyDev("wrong", tok); err == nil {
		t.Fatal("want signature error")
	}
}

func TestMiddlewareDev(t *testing.T) {
	t.Setenv("AUTH_MODE", "dev")
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := FromContext(r)
		if !ok && r.URL.Path != "/healthz" {
			t.Error("no claims")
		}
		fmt.Fprint(w, c.Sub)
	}))
	// healthz public
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz: %d", rec.Code)
	}
	// X-Dev-Role
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/x", nil)
	req.Header.Set("X-Dev-Role", "auditor")
	h.ServeHTTP(rec, req)
	if rec.Body.String() != "dev-auditor" {
		t.Fatalf("dev role: %s", rec.Body.String())
	}
	// no auth -> 401
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/x", nil))
	if rec.Code != 401 {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}
