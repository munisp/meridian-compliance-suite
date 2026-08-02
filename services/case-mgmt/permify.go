// permify.go — P0: centralized authorization via the Permify server.
//
// Env-selected like the other middleware (HARDENING H1/H3):
//   - PERMIFY_URL set   -> live Permify Check API (POST
//     /v1/tenants/{tenant}/permissions/check); the dev file-backed
//     RelationChecker is bypassed for checks, and relation grant/revoke
//     endpoints return 501 (tuples must be written to Permify directly).
//   - PERMIFY_URL unset -> existing dev file-backed checker, honest log.
//   - PROFILE=prod (or AUTH_MODE=keycloak) + PERMIFY_URL unset -> startup
//     FAILS CLOSED: no silent decentralized authz in prod.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// PermifyClient is a thin stdlib client for the Permify Check API v1.
type PermifyClient struct {
	base    string
	tenant  string
	hc      *http.Client
	timeout time.Duration
}

// NewPermifyClient builds a client for the server at baseURL.
func NewPermifyClient(baseURL, tenant string) *PermifyClient {
	if tenant == "" {
		tenant = "t1"
	}
	return &PermifyClient{
		base:    strings.TrimRight(baseURL, "/"),
		tenant:  tenant,
		hc:      &http.Client{},
		timeout: 2 * time.Second,
	}
}

// permifyClientFromEnv returns a live client when PERMIFY_URL is set, else
// nil (dev file-backed fallback).
func permifyClientFromEnv() *PermifyClient {
	base := os.Getenv("PERMIFY_URL")
	if base == "" {
		return nil
	}
	return NewPermifyClient(base, os.Getenv("PERMIFY_TENANT"))
}

type permifyRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

func splitPermifyRef(s string) (permifyRef, error) {
	i := strings.Index(s, ":")
	if i <= 0 || i == len(s)-1 {
		return permifyRef{}, fmt.Errorf("permify reference %q must be type:id", s)
	}
	return permifyRef{Type: s[:i], ID: s[i+1:]}, nil
}

// Check reports whether subject holds entity#permission. One retry on 5xx,
// 2s timeout, failures are circuit-logged and returned as errors (callers
// fail closed).
func (c *PermifyClient) Check(ctx context.Context, entity, permission, subject string) (bool, error) {
	ent, err := splitPermifyRef(entity)
	if err != nil {
		return false, err
	}
	sub, err := splitPermifyRef(subject)
	if err != nil {
		return false, err
	}
	body, _ := json.Marshal(map[string]any{
		"entity": ent, "permission": permission, "subject": sub,
	})
	url := c.base + "/v1/tenants/" + c.tenant + "/permissions/check"

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		allowed, retryable, err := c.do(ctx, url, body)
		if err == nil {
			return allowed, nil
		}
		lastErr = err
		log.Printf("component=permify circuit: check %s#%s@%s attempt %d failed: %v",
			entity, permission, subject, attempt+1, err)
		if !retryable {
			break
		}
	}
	return false, lastErr
}

func (c *PermifyClient) do(ctx context.Context, url string, body []byte) (allowed, retryable bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return false, false, fmt.Errorf("permify check transport: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 500 {
		return false, true, fmt.Errorf("permify check status %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return false, false, fmt.Errorf("permify check status %d", resp.StatusCode)
	}
	var out struct {
		Can     string `json:"can"`
		Allowed *bool  `json:"allowed"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, false, fmt.Errorf("permify check decode: %w", err)
	}
	if out.Allowed != nil {
		return *out.Allowed, false, nil
	}
	return out.Can == "RESULT_ALLOWED", false, nil
}

// wirePermify selects the authz backend. Returns (client, error); error only
// for the prod fail-closed case (main log.Fatals).
func wirePermify(authMode string) (*PermifyClient, error) {
	if c := permifyClientFromEnv(); c != nil {
		log.Printf("component=case-mgmt permify=live url=%s tenant=%s",
			os.Getenv("PERMIFY_URL"), env("PERMIFY_TENANT", "t1"))
		return c, nil
	}
	if os.Getenv("PROFILE") == "prod" || authMode == "keycloak" {
		return nil, fmt.Errorf("PERMIFY_URL is required when PROFILE=prod or AUTH_MODE=keycloak (centralized authz fail-closed; refusing the dev file-backed checker)")
	}
	log.Printf("profile=dev component=case-mgmt WARNING: PERMIFY_URL unset; using dev file-backed relation checker (Permify not consulted)")
	return nil, nil
}

// checkRel routes a relation check: live Permify when wired, else the dev
// file-backed checker. Permify errors fail closed (deny).
func (s *Service) checkRel(r *http.Request, entity, permission, subject string) bool {
	if s.perm != nil {
		allowed, err := s.perm.Check(r.Context(), entity, permission, subject)
		if err != nil {
			log.Printf("component=case-mgmt permify check failed (%v); request denied (fail-closed)", err)
			return false
		}
		return allowed
	}
	return s.rel.Check(entity, permission, subject, s.store)
}

// relCheckerMode labels the active checker for honest API responses.
func (s *Service) relCheckerMode() string {
	if s.perm != nil {
		return "permify-live"
	}
	return "dev-file-backed"
}
