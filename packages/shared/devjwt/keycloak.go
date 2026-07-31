package devjwt

import (
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/munisp/meridian-compliance-suite/packages/authx"
)

// Keycloak-aware middleware selection (HARDENING.md H2): AUTH_MODE=keycloak
// verifies RS256 tokens against the realm JWKS (KEYCLOAK_ISSUER /
// KEYCLOAK_AUDIENCE / KEYCLOAK_JWKS_URL) and maps Keycloak realm/client roles
// into the dev Claims shape, so handlers keep using devjwt.FromContext.
// Dev mode (default) is unchanged: HS256 + X-Dev-Role. Startup never fails
// when a prod var is missing — it falls back to dev with a profile=dev log.

func claimsFromAuthx(c authx.Claims) Claims {
	return Claims{Sub: c.Sub, Roles: c.Roles, TenantID: c.TenantID, Exp: c.Exp, Iat: c.Iat}
}

// MiddlewareEnv is Middleware with AUTH_MODE selection (keycloak|dev).
// Middleware keeps its historic behaviour (dev) for zero-config compat; new
// services should use MiddlewareEnv. Middleware itself now delegates to
// MiddlewareEnv so existing call sites gain keycloak support automatically.
func MiddlewareEnv(next http.Handler) http.Handler {
	if os.Getenv("AUTH_MODE") == "keycloak" {
		// FAIL CLOSED (audit fix): a keycloak deployment with missing
		// configuration must refuse to boot rather than silently run dev auth
		// (hardcoded HS256 secret / X-Dev-Role header) in production.
		v := authx.KeycloakVerifierFromEnv()
		if v == nil {
			log.Fatal("AUTH_MODE=keycloak but KEYCLOAK_ISSUER/JWKS configuration is incomplete; refusing to start (no dev fallback)")
		}
		log.Printf("profile=prod component=auth (keycloak issuer=%s)", v.Issuer)
		return keycloakMiddleware(next, v)
	}
	log.Printf("profile=dev component=auth")
	return devMiddleware(next)
}

func keycloakMiddleware(next http.Handler, v *authx.KeycloakVerifier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			c, err := v.Verify(strings.TrimPrefix(auth, "Bearer "))
			if err != nil {
				Problem(w, http.StatusUnauthorized, "unauthorized", err.Error())
				return
			}
			next.ServeHTTP(w, r.WithContext(withClaims(r, claimsFromAuthx(c))))
			return
		}
		Problem(w, http.StatusUnauthorized, "unauthorized", "Bearer JWT required (AUTH_MODE=keycloak)")
	})
}

func devMiddleware(next http.Handler) http.Handler {
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
		switch role := r.Header.Get("X-Dev-Role"); role {
		case "admin", "operator", "auditor":
			c := Claims{Sub: "dev-" + role, Roles: []string{role}, TenantID: r.Header.Get("X-Tenant-ID")}
			next.ServeHTTP(w, r.WithContext(withClaims(r, c)))
			return
		}
		Problem(w, http.StatusUnauthorized, "unauthorized", "provide Bearer JWT or X-Dev-Role (dev mode)")
	})
}
