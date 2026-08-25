// apikeys.go — merchant self-service API key lifecycle (feature I5).
// Keys authenticate machine-to-machine calls as an alternative to the dev
// JWT (X-Api-Key header). The plaintext secret is returned exactly once at
// create/rotate time; at rest only the SHA-256 hash and a short
// non-sensitive lookup prefix are persisted. Verification uses a
// constant-time comparison (hmac.Equal over the digests). All lifecycle
// operations are tenant-scoped (caller tenant only), role-gated
// (admin/operator) and audit-logged per the platform structured-log
// convention (component=... audit ...).
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/munisp/meridian-compliance-suite/packages/shared/devjwt"
)

// apiKeyPrefix identifies Meridian self-service keys; the lookup prefix
// stored alongside the hash is the first 8 chars of the secret body (not
// the whole secret — enough to find candidate rows, useless on its own).
const apiKeyPrefix = "mrk_"

// APIKey is the durable record. Plaintext is NEVER stored here.
type APIKey struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"tenant_id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"` // mrk_ + 8 lookup chars; safe to display
	Hash       string     `json:"hash"`   // hex SHA-256 of the full plaintext key
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	RotatedAt  *time.Time `json:"rotated_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// Active reports whether the key can authenticate requests.
func (k *APIKey) Active() bool { return k.RevokedAt == nil }

// public redacts the hash for list/metadata responses.
func (k *APIKey) public() map[string]any {
	return map[string]any{
		"id": k.ID, "tenant_id": k.TenantID, "name": k.Name, "prefix": k.Prefix,
		"created_by": k.CreatedBy, "created_at": k.CreatedAt,
		"rotated_at": k.RotatedAt, "revoked_at": k.RevokedAt,
		"last_used_at": k.LastUsedAt, "active": k.Active(),
	}
}

// APIKeyStore is the durable key store (dev: JSONL file + in-memory index,
// mirroring InvoiceStore; snapshot rewrite on each mutation — key volume is
// small and every mutation is an audit-worthy event).
type APIKeyStore struct {
	path     string
	mu       sync.RWMutex
	byID     map[string]*APIKey
	byPrefix map[string][]string // lookup prefix -> key ids
}

func NewAPIKeyStore(path string) (*APIKeyStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &APIKeyStore{path: path, byID: map[string]*APIKey{}, byPrefix: map[string][]string{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *APIKeyStore) load() error {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for dec.More() {
		var k APIKey
		if err := dec.Decode(&k); err != nil {
			return fmt.Errorf("apikey store corrupt: %w", err)
		}
		kk := k
		s.byID[kk.ID] = &kk
		s.byPrefix[kk.Prefix] = append(s.byPrefix[kk.Prefix], kk.ID)
	}
	return nil
}

// persist rewrites the JSONL snapshot (last write wins by id on load).
func (s *APIKeyStore) persistLocked() error {
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(s.byID))
	for id := range s.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	enc := json.NewEncoder(f)
	for _, id := range ids {
		if err := enc.Encode(s.byID[id]); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// generateSecret mints a new plaintext key: mrk_ + 32 random bytes (hex).
func generateSecret() (plaintext, lookupPrefix, hash string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", err
	}
	plaintext = apiKeyPrefix + hex.EncodeToString(buf)
	lookupPrefix = plaintext[:len(apiKeyPrefix)+8]
	sum := sha256.Sum256([]byte(plaintext))
	hash = hex.EncodeToString(sum[:])
	return plaintext, lookupPrefix, hash, nil
}

// Create mints a key for the tenant and returns the plaintext ONCE.
func (s *APIKeyStore) Create(tenantID, name, actor string) (*APIKey, string, error) {
	plain, prefix, hash, err := generateSecret()
	if err != nil {
		return nil, "", err
	}
	idBuf := make([]byte, 8)
	if _, err := rand.Read(idBuf); err != nil {
		return nil, "", err
	}
	k := &APIKey{
		ID: "key_" + hex.EncodeToString(idBuf), TenantID: tenantID, Name: name,
		Prefix: prefix, Hash: hash, CreatedBy: actor, CreatedAt: time.Now().UTC(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[k.ID] = k
	s.byPrefix[prefix] = append(s.byPrefix[prefix], k.ID)
	if err := s.persistLocked(); err != nil {
		delete(s.byID, k.ID)
		return nil, "", err
	}
	return k, plain, nil
}

// List returns the tenant's keys (metadata only — hashes never leave).
func (s *APIKeyStore) List(tenantID string) []*APIKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*APIKey{}
	for _, k := range s.byID {
		if k.TenantID == tenantID {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Rotate replaces the secret of key id (tenant-scoped): new plaintext is
// returned once; the old hash is destroyed so the old secret dies
// immediately.
func (s *APIKeyStore) Rotate(tenantID, id string) (*APIKey, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok || k.TenantID != tenantID {
		return nil, "", os.ErrNotExist
	}
	if !k.Active() {
		return nil, "", errors.New("key is revoked")
	}
	plain, prefix, hash, err := generateSecret()
	if err != nil {
		return nil, "", err
	}
	// re-index prefix
	old := s.byPrefix[k.Prefix]
	for i, kid := range old {
		if kid == id {
			s.byPrefix[k.Prefix] = append(old[:i], old[i+1:]...)
			break
		}
	}
	k.Prefix, k.Hash = prefix, hash
	now := time.Now().UTC()
	k.RotatedAt = &now
	s.byPrefix[prefix] = append(s.byPrefix[prefix], id)
	if err := s.persistLocked(); err != nil {
		return nil, "", err
	}
	return k, plain, nil
}

// Revoke marks the key unusable (tenant-scoped, idempotent-safe).
func (s *APIKeyStore) Revoke(tenantID, id string) (*APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok || k.TenantID != tenantID {
		return nil, os.ErrNotExist
	}
	if k.RevokedAt == nil {
		now := time.Now().UTC()
		k.RevokedAt = &now
		if err := s.persistLocked(); err != nil {
			return nil, err
		}
	}
	return k, nil
}

// Verify authenticates a presented plaintext key. The digest comparison is
// constant-time (hmac.Equal) against every candidate sharing the lookup
// prefix, so timing reveals neither the hash nor whether a prefix exists.
// On success the key record (with LastUsedAt bumped) is returned.
func (s *APIKeyStore) Verify(plaintext string) (*APIKey, bool) {
	if !strings.HasPrefix(plaintext, apiKeyPrefix) || len(plaintext) < len(apiKeyPrefix)+8 {
		return nil, false
	}
	sum := sha256.Sum256([]byte(plaintext))
	prefix := plaintext[:len(apiKeyPrefix)+8]
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched *APIKey
	for _, id := range s.byPrefix[prefix] {
		k := s.byID[id]
		stored, err := hex.DecodeString(k.Hash)
		if err != nil || len(stored) != len(sum) {
			continue
		}
		if hmac.Equal(stored, sum[:]) {
			matched = k
		}
	}
	if matched == nil || !matched.Active() {
		return nil, false
	}
	now := time.Now().UTC()
	matched.LastUsedAt = &now
	_ = s.persistLocked() // best-effort usage stamp; auth must not fail on IO
	return matched, true
}

// apiKeyMiddleware accepts X-Api-Key as an alternative principal to the JWT
// middleware: requests WITH the header are verified against the key store
// and, on success, served by `authed` (the route mux) carrying an
// operator-role claim scoped to the key's tenant (used by machine
// integrations on the invoice-create route). A present-but-invalid key is a
// 401 — never a silent fallthrough. Requests without the header go through
// `fallback` (the standard JWT/dev middleware chain) unchanged.
func (s *Server) apiKeyMiddleware(authed, fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Api-Key")
		if key == "" {
			fallback.ServeHTTP(w, r)
			return
		}
		k, ok := s.apiKeys.Verify(key)
		if !ok {
			log.Printf("component=einvoicing audit apikey-auth outcome=denied prefix=%q path=%s", keyPrefixForLog(key), r.URL.Path)
			devjwt.Problem(w, http.StatusUnauthorized, "unauthorized", "invalid or revoked API key")
			return
		}
		c := devjwt.Claims{
			Sub:      "apikey:" + k.ID,
			Roles:    []string{"operator"},
			TenantID: k.TenantID,
		}
		authed.ServeHTTP(w, devjwt.WithClaims(r, c))
	})
}

func keyPrefixForLog(key string) string {
	if len(key) > len(apiKeyPrefix)+8 {
		return key[:len(apiKeyPrefix)+8]
	}
	return key
}

// requireSelfService gates lifecycle endpoints: authenticated admin/operator
// with a tenant claim (tenant scoping is never void).
func requireSelfService(w http.ResponseWriter, r *http.Request) (devjwt.Claims, bool) {
	claims, ok := devjwt.FromContext(r)
	if !ok || claims.Sub == "" {
		devjwt.Problem(w, 401, "unauthorized", "authentication required")
		return devjwt.Claims{}, false
	}
	if !hasAnyRole(claims.Roles, "admin", "operator") {
		devjwt.Problem(w, 403, "forbidden", "admin or operator role required")
		return devjwt.Claims{}, false
	}
	if claims.TenantID == "" {
		devjwt.Problem(w, 403, "forbidden", "tenant claim required")
		return devjwt.Claims{}, false
	}
	return claims, true
}

// handleAPIKeyCreate mints a key; plaintext is in the response exactly once.
func (s *Server) handleAPIKeyCreate(w http.ResponseWriter, r *http.Request) {
	claims, ok := requireSelfService(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		devjwt.Problem(w, 400, "bad request", err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 120 {
		devjwt.Problem(w, 422, "invalid name", "name is required (max 120 chars)")
		return
	}
	k, plain, err := s.apiKeys.Create(claims.TenantID, req.Name, claims.Sub)
	if err != nil {
		devjwt.Problem(w, 500, "persist failed", err.Error())
		return
	}
	log.Printf("component=einvoicing audit apikey-create actor=%s tenant=%s key_id=%s prefix=%s", claims.Sub, claims.TenantID, k.ID, k.Prefix)
	out := k.public()
	out["api_key"] = plain // shown once, never stored
	writeJSON(w, 201, out)
}

// handleAPIKeyList returns the caller tenant's keys (metadata only).
func (s *Server) handleAPIKeyList(w http.ResponseWriter, r *http.Request) {
	claims, ok := requireSelfService(w, r)
	if !ok {
		return
	}
	keys := s.apiKeys.List(claims.TenantID)
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.public())
	}
	writeJSON(w, 200, map[string]any{"keys": out, "count": len(out)})
}

// handleAPIKeyRotate issues a new secret for an existing key id.
func (s *Server) handleAPIKeyRotate(w http.ResponseWriter, r *http.Request) {
	claims, ok := requireSelfService(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	k, plain, err := s.apiKeys.Rotate(claims.TenantID, id)
	if errors.Is(err, os.ErrNotExist) {
		// 404 for cross-tenant and unknown alike — no existence oracle.
		devjwt.Problem(w, 404, "not found", "api key "+id)
		return
	}
	if err != nil {
		devjwt.Problem(w, 409, "conflict", err.Error())
		return
	}
	log.Printf("component=einvoicing audit apikey-rotate actor=%s tenant=%s key_id=%s prefix=%s", claims.Sub, claims.TenantID, k.ID, k.Prefix)
	out := k.public()
	out["api_key"] = plain // shown once, never stored
	writeJSON(w, 200, out)
}

// handleAPIKeyRevoke permanently disables a key.
func (s *Server) handleAPIKeyRevoke(w http.ResponseWriter, r *http.Request) {
	claims, ok := requireSelfService(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	k, err := s.apiKeys.Revoke(claims.TenantID, id)
	if errors.Is(err, os.ErrNotExist) {
		devjwt.Problem(w, 404, "not found", "api key "+id)
		return
	}
	if err != nil {
		devjwt.Problem(w, 500, "persist failed", err.Error())
		return
	}
	log.Printf("component=einvoicing audit apikey-revoke actor=%s tenant=%s key_id=%s", claims.Sub, claims.TenantID, k.ID)
	writeJSON(w, 200, k.public())
}
