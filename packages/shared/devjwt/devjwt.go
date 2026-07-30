// Package devjwt implements SPEC §1.3 auth: HS256 dev JWT (secret from
// MERIDIAN_DEV_JWT_SECRET; claims sub/roles/tenant_id) plus the AUTH_MODE=dev
// X-Dev-Role header. Zero external deps (manual JWT over crypto/hmac).
package devjwt

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"
)

// Claims carried in the dev JWT.
type Claims struct {
	Sub      string   `json:"sub"`
	Roles    []string `json:"roles"`
	TenantID string   `json:"tenant_id"`
	Exp      int64    `json:"exp,omitempty"`
	Iat      int64    `json:"iat,omitempty"`
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// Secret returns the configured dev secret or a documented localhost default.
func Secret() string {
	if s := os.Getenv("MERIDIAN_DEV_JWT_SECRET"); s != "" {
		return s
	}
	return "meridian-dev-secret-change-me-32!"
}

// Issue signs an HS256 JWT.
func Issue(secret string, c Claims) (string, error) {
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

// Verify parses and validates an HS256 JWT.
func Verify(secret, token string) (Claims, error) {
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

type ctxKey struct{}

// FromContext returns the authenticated principal (sub) and roles.
func FromContext(r *http.Request) (Claims, bool) {
	c, ok := r.Context().Value(ctxKey{}).(Claims)
	return c, ok
}

// Problem writes an RFC7807 problem+json response.
func Problem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "about:blank",
		"title":  title,
		"status": status,
		"detail": detail,
	})
}

// Middleware enforces Bearer JWT, or X-Dev-Role when AUTH_MODE=dev.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			c, err := Verify(Secret(), strings.TrimPrefix(auth, "Bearer "))
			if err != nil {
				Problem(w, http.StatusUnauthorized, "unauthorized", err.Error())
				return
			}
			next.ServeHTTP(w, r.WithContext(withClaims(r, c)))
			return
		}
		if os.Getenv("AUTH_MODE") == "dev" || os.Getenv("AUTH_MODE") == "" {
			role := r.Header.Get("X-Dev-Role")
			switch role {
			case "admin", "operator", "auditor":
				c := Claims{Sub: "dev-" + role, Roles: []string{role}, TenantID: r.Header.Get("X-Tenant-ID")}
				next.ServeHTTP(w, r.WithContext(withClaims(r, c)))
				return
			}
		}
		Problem(w, http.StatusUnauthorized, "unauthorized", "provide Bearer JWT or X-Dev-Role (dev mode)")
	})
}

func withClaims(r *http.Request, c Claims) context.Context {
	return context.WithValue(r.Context(), ctxKey{}, c)
}
