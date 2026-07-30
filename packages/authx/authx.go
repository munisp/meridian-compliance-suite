// Package authx is the compact shared auth package for the Meridian
// compliance-suite services (HARDENING.md H2). It provides:
//
//   - Dev verifier: HS256 JWT (MERIDIAN_DEV_JWT_SECRET) + X-Dev-Role header
//     when AUTH_MODE=dev (default). Byte-identical semantics to the historic
//     per-service verifiers.
//   - Keycloak verifier: RS256 JWT verified against the realm JWKS
//     (KEYCLOAK_JWKS_URL, default {KEYCLOAK_ISSUER}/protocol/openid-connect/certs)
//     with a 5-minute cache and refresh-on-unknown-kid; validates iss/exp/aud
//     (KEYCLOAK_AUDIENCE) and maps Keycloak realm_access.roles plus
//     resource_access[audience].roles into the flat Roles claim. Accepts
//     service-to-service client-credentials tokens (aud=KEYCLOAK_AUDIENCE).
//
// Selection is purely via AUTH_MODE; services switch with zero code change:
//
//	http.ListenAndServe(addr, authx.Middleware(mux))
//
// Startup never fails because a prod var is missing: when AUTH_MODE=keycloak
// but KEYCLOAK_ISSUER is unset the middleware falls back to dev behaviour and
// logs profile=dev component=auth.
package authx

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Claims is the authenticated principal shape shared by dev and keycloak
// modes (SPEC §1.3).
type Claims struct {
	Sub      string   `json:"sub"`
	Roles    []string `json:"roles"`
	TenantID string   `json:"tenant_id"`
	Exp      int64    `json:"exp,omitempty"`
	Iat      int64    `json:"iat,omitempty"`
}

type ctxKey struct{}

// FromContext returns the authenticated principal, if any.
func FromContext(r *http.Request) (Claims, bool) {
	c, ok := r.Context().Value(ctxKey{}).(Claims)
	return c, ok
}

func withClaims(r *http.Request, c Claims) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, c))
}

// Problem writes an RFC7807 problem+json response.
func Problem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "about:blank", "title": title, "status": status, "detail": detail,
	})
}

// DevSecret returns the dev HMAC secret or the documented default.
func DevSecret() string {
	if s := os.Getenv("MERIDIAN_DEV_JWT_SECRET"); s != "" {
		return s
	}
	return "meridian-dev-secret-change-me-32!"
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// VerifyDev validates an HS256 dev token.
func VerifyDev(secret, token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("malformed token")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	sig, err := unb64(parts[2])
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return Claims{}, errors.New("bad signature")
	}
	pl, err := unb64(parts[1])
	if err != nil {
		return Claims{}, err
	}
	var c Claims
	if err := json.Unmarshal(pl, &c); err != nil {
		return Claims{}, err
	}
	if c.Exp != 0 && time.Now().Unix() > c.Exp {
		return Claims{}, errors.New("token expired")
	}
	return c, nil
}

// IssueDev signs an HS256 dev token (tests/tooling).
func IssueDev(secret string, c Claims) (string, error) {
	if c.Iat == 0 {
		c.Iat = time.Now().Unix()
	}
	if c.Exp == 0 {
		c.Exp = time.Now().Add(8 * time.Hour).Unix()
	}
	hdr, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	pl, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	msg := b64(hdr) + "." + b64(pl)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg))
	return msg + "." + b64(mac.Sum(nil)), nil
}

// ---------------- Keycloak RS256 / JWKS ----------------

type jwksKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwksDoc struct {
	Keys []jwksKey `json:"keys"`
}

// KeycloakVerifier verifies RS256 tokens against a Keycloak realm JWKS.
type KeycloakVerifier struct {
	Issuer   string
	Audience string
	JWKSURL  string

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	httpc     *http.Client
}

// NewKeycloakVerifier builds a verifier from explicit settings. jwksURL may
// be empty, in which case it is derived from the issuer.
func NewKeycloakVerifier(issuer, audience, jwksURL string) *KeycloakVerifier {
	if jwksURL == "" {
		jwksURL = strings.TrimSuffix(issuer, "/") + "/protocol/openid-connect/certs"
	}
	return &KeycloakVerifier{
		Issuer: issuer, Audience: audience, JWKSURL: jwksURL,
		keys:  map[string]*rsa.PublicKey{},
		httpc: &http.Client{Timeout: 10 * time.Second},
	}
}

// KeycloakVerifierFromEnv returns a verifier from KEYCLOAK_* env vars, or nil
// when KEYCLOAK_ISSUER is unset (dev fallback).
func KeycloakVerifierFromEnv() *KeycloakVerifier {
	issuer := os.Getenv("KEYCLOAK_ISSUER")
	if issuer == "" {
		return nil
	}
	return NewKeycloakVerifier(issuer, os.Getenv("KEYCLOAK_AUDIENCE"), os.Getenv("KEYCLOAK_JWKS_URL"))
}

const jwksCacheTTL = 5 * time.Minute

func (v *KeycloakVerifier) fetchJWKS() error {
	resp, err := v.httpc.Get(v.JWKSURL)
	if err != nil {
		return fmt.Errorf("jwks fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks fetch: status %d", resp.StatusCode)
	}
	var doc jwksDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("jwks decode: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nb, err := unb64(k.N)
		if err != nil {
			continue
		}
		eb, err := unb64(k.E)
		if err != nil {
			continue
		}
		e := 0
		for _, b := range eb {
			e = e<<8 | int(b)
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}
	}
	v.keys = keys
	v.fetchedAt = time.Now()
	return nil
}

func (v *KeycloakVerifier) keyFor(kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok && time.Since(v.fetchedAt) < jwksCacheTTL {
		return k, nil
	}
	// Refresh on unknown kid or stale cache.
	if err := v.fetchJWKS(); err != nil {
		// Serve stale key if we have one.
		if k, ok := v.keys[kid]; ok {
			return k, nil
		}
		return nil, err
	}
	k, ok := v.keys[kid]
	if !ok {
		return nil, fmt.Errorf("unknown kid %q", kid)
	}
	return k, nil
}

type kcClaims struct {
	Sub      string `json:"sub"`
	Iss      string `json:"iss"`
	Aud      any    `json:"aud"` // string or []string
	Exp      int64  `json:"exp"`
	Iat      int64  `json:"iat"`
	TenantID string `json:"tenant_id"`
	Realm    struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
	Resource map[string]struct {
		Roles []string `json:"roles"`
	} `json:"resource_access"`
}

func audStrings(a any) []string {
	switch v := a.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Verify validates an RS256 Keycloak token: signature (JWKS), iss, exp, aud,
// and maps realm + client roles into Claims.Roles.
func (v *KeycloakVerifier) Verify(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("malformed token")
	}
	hdrBytes, err := unb64(parts[0])
	if err != nil {
		return Claims{}, err
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return Claims{}, err
	}
	if hdr.Alg != "RS256" {
		return Claims{}, fmt.Errorf("unexpected alg %q (want RS256)", hdr.Alg)
	}
	key, err := v.keyFor(hdr.Kid)
	if err != nil {
		return Claims{}, err
	}
	sig, err := unb64(parts[2])
	if err != nil {
		return Claims{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return Claims{}, errors.New("bad signature")
	}
	pl, err := unb64(parts[1])
	if err != nil {
		return Claims{}, err
	}
	var kc kcClaims
	if err := json.Unmarshal(pl, &kc); err != nil {
		return Claims{}, err
	}
	if v.Issuer != "" && kc.Iss != strings.TrimSuffix(v.Issuer, "/") && kc.Iss != v.Issuer {
		return Claims{}, fmt.Errorf("bad issuer %q", kc.Iss)
	}
	if kc.Exp == 0 || time.Now().Unix() > kc.Exp {
		return Claims{}, errors.New("token expired")
	}
	if v.Audience != "" {
		ok := false
		for _, a := range audStrings(kc.Aud) {
			if a == v.Audience {
				ok = true
			}
		}
		if !ok {
			return Claims{}, fmt.Errorf("audience %v not accepted", audStrings(kc.Aud))
		}
	}
	roles := append([]string{}, kc.Realm.Roles...)
	if v.Audience != "" {
		if ra, ok := kc.Resource[v.Audience]; ok {
			roles = append(roles, ra.Roles...)
		}
	}
	return Claims{Sub: kc.Sub, Roles: roles, TenantID: kc.TenantID, Exp: kc.Exp, Iat: kc.Iat}, nil
}

// ---------------- env-selected middleware ----------------

// Middleware enforces Bearer JWT per AUTH_MODE (H1/H2): keycloak mode uses
// RS256/JWKS; dev mode (default) accepts HS256 dev tokens and X-Dev-Role.
// /healthz and /readyz are always public.
func Middleware(next http.Handler) http.Handler {
	mode := os.Getenv("AUTH_MODE")
	var kc *KeycloakVerifier
	if mode == "keycloak" {
		kc = KeycloakVerifierFromEnv()
		if kc == nil {
			log.Printf("profile=dev component=auth (AUTH_MODE=keycloak but KEYCLOAK_ISSUER unset)")
			mode = "dev"
		} else {
			log.Printf("profile=prod component=auth (keycloak issuer=%s)", kc.Issuer)
		}
	} else {
		log.Printf("profile=dev component=auth")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			tok := strings.TrimPrefix(auth, "Bearer ")
			var c Claims
			var err error
			if mode == "keycloak" {
				c, err = kc.Verify(tok)
			} else {
				c, err = VerifyDev(DevSecret(), tok)
			}
			if err != nil {
				Problem(w, http.StatusUnauthorized, "unauthorized", err.Error())
				return
			}
			next.ServeHTTP(w, withClaims(r, c))
			return
		}
		if mode == "dev" {
			switch role := r.Header.Get("X-Dev-Role"); role {
			case "admin", "operator", "auditor":
				c := Claims{Sub: "dev-" + role, Roles: []string{role}, TenantID: r.Header.Get("X-Tenant-ID")}
				next.ServeHTTP(w, withClaims(r, c))
				return
			}
		}
		Problem(w, http.StatusUnauthorized, "unauthorized", "provide Bearer JWT or X-Dev-Role (dev mode)")
	})
}
