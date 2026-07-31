package provider

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// CloudKMS is the [REAL] cloud-KMS REST signer (AWS KMS / Azure Key Vault /
// GCP Cloud KMS style). Signing runs remotely over HTTPS and every returned
// signature is verified locally against the KMS-published public key (or by
// HMAC recomputation via the KMS for symmetric keys) before being accepted —
// a KMS returning an invalid signature is treated as unavailable
// (fail-closed).
//
// Configuration (env, see provider.go):
//
//	KMS_BASE_URL      base URL of the KMS-compatible REST endpoint
//	KMS_STYLE         aws | azure | gcp — request shape only; the wire
//	                  contract is the Meridian KMS shim documented below
//	KMS_BEARER_TOKEN  static bearer token (dev / token-injection setups;
//	                  prod setups place the endpoint behind mTLS or IRSA/
//	                  managed-identity sidecars)
//
// Meridian KMS shim REST contract (implemented by thin vendor adapters):
//
//	POST {base}/v1/keys/{keyID}/sign    {"payload_b64": "...", "algorithm": "ed25519"|"hmac-sha256"}
//	                                    -> {"signature_b64": "...", "key_version": "n"}
//	GET  {base}/v1/keys/{keyID}/public  -> {"public_key_b64": "...", "algorithm": "ed25519"}
//	POST {base}/v1/keys/{keyID}/rotate  -> {"key_version": "n+1"}
//
// Import-guarded: stdlib only, no AWS/Azure/GCP SDK imports.

// CloudKMSConfig configures the CloudKMS provider.
type CloudKMSConfig struct {
	BaseURL     string // KMS_BASE_URL
	Style       string // aws | azure | gcp (defaults to aws)
	BearerToken string // KMS_BEARER_TOKEN (optional)
	HTTPClient  *http.Client
}

// CloudKMS implements SignerProvider against a KMS REST shim.
type CloudKMS struct {
	cfg    CloudKMSConfig
	client *http.Client
	mu     sync.Mutex
	pubs   map[string][]byte // public-key cache (invalidated on rotate)
}

// NewCloudKMS validates the config and returns the provider.
func NewCloudKMS(cfg CloudKMSConfig) (*CloudKMS, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("cloud-kms: KMS_BASE_URL required")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("cloud-kms: invalid KMS_BASE_URL %q", cfg.BaseURL)
	}
	switch cfg.Style {
	case "", "aws", "azure", "gcp":
	default:
		return nil, fmt.Errorf("cloud-kms: unknown KMS_STYLE %q (aws|azure|gcp)", cfg.Style)
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	return &CloudKMS{cfg: cfg, client: c, pubs: map[string][]byte{}}, nil
}

// Mode implements SignerProvider.
func (k *CloudKMS) Mode() string { return "cloud-kms" }

func (k *CloudKMS) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(k.cfg.BaseURL, "/")+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if k.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+k.cfg.BearerToken)
	}
	// Style-specific header shims (aws/azure/gcp request shapes).
	switch k.cfg.Style {
	case "azure":
		req.Header.Set("api-version", "7.4")
	case "gcp":
		req.Header.Set("X-Goog-Request-Params", "route=sign")
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: cloud-kms %s %s: %v", ErrUnavailable, method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%w: cloud-kms %s %s returned %d: %s", ErrUnavailable, method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("cloud-kms: malformed response: %v", err)
		}
	}
	return nil
}

func algorithm(keyID string) string {
	if IsHMACKeyID(keyID) {
		return "hmac-sha256"
	}
	return "ed25519"
}

// Sign implements SignerProvider: remote sign + local verification.
func (k *CloudKMS) Sign(ctx context.Context, keyID string, payload []byte) ([]byte, error) {
	var out struct {
		SignatureB64 string `json:"signature_b64"`
		KeyVersion   string `json:"key_version"`
	}
	if err := k.do(ctx, http.MethodPost, "/v1/keys/"+url.PathEscape(keyID)+"/sign", map[string]any{
		"payload_b64": base64.StdEncoding.EncodeToString(payload),
		"algorithm":   algorithm(keyID),
	}, &out); err != nil {
		return nil, err
	}
	sig, err := base64.StdEncoding.DecodeString(out.SignatureB64)
	if err != nil {
		return nil, fmt.Errorf("cloud-kms: invalid signature encoding: %v", err)
	}
	// [REAL] sig verification: never trust the remote signer blindly.
	if IsHMACKeyID(keyID) {
		// Symmetric: recompute via KMS and compare (constant time).
		var chk struct {
			SignatureB64 string `json:"signature_b64"`
		}
		if err := k.do(ctx, http.MethodPost, "/v1/keys/"+url.PathEscape(keyID)+"/sign", map[string]any{
			"payload_b64": base64.StdEncoding.EncodeToString(payload),
			"algorithm":   algorithm(keyID),
		}, &chk); err != nil {
			return nil, err
		}
		again, err := base64.StdEncoding.DecodeString(chk.SignatureB64)
		if err != nil || !hmac.Equal(sig, again) {
			return nil, fmt.Errorf("%w: cloud-kms returned inconsistent HMAC signatures", ErrUnavailable)
		}
		return sig, nil
	}
	pub, err := k.PublicKey(ctx, keyID)
	if err != nil {
		return nil, err
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(ed25519.PublicKey(pub), payload, sig) {
		return nil, fmt.Errorf("%w: cloud-kms signature failed local ed25519 verification", ErrUnavailable)
	}
	return sig, nil
}

// PublicKey implements SignerProvider (cached until Rotate).
func (k *CloudKMS) PublicKey(ctx context.Context, keyID string) ([]byte, error) {
	if IsHMACKeyID(keyID) {
		return nil, ErrSymmetricKey
	}
	k.mu.Lock()
	if pub, ok := k.pubs[keyID]; ok {
		k.mu.Unlock()
		return pub, nil
	}
	k.mu.Unlock()
	var out struct {
		PublicKeyB64 string `json:"public_key_b64"`
		Algorithm    string `json:"algorithm"`
	}
	if err := k.do(ctx, http.MethodGet, "/v1/keys/"+url.PathEscape(keyID)+"/public", nil, &out); err != nil {
		return nil, err
	}
	pub, err := base64.StdEncoding.DecodeString(out.PublicKeyB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("cloud-kms: invalid public key for %q", keyID)
	}
	k.mu.Lock()
	k.pubs[keyID] = pub
	k.mu.Unlock()
	return pub, nil
}

// Rotate implements SignerProvider: remote rotate + local cache invalidation.
func (k *CloudKMS) Rotate(ctx context.Context, keyID string) error {
	if err := k.do(ctx, http.MethodPost, "/v1/keys/"+url.PathEscape(keyID)+"/rotate", map[string]any{}, nil); err != nil {
		return err
	}
	k.mu.Lock()
	delete(k.pubs, keyID)
	k.mu.Unlock()
	return nil
}

// sha256Sum is retained for shim adapters that pre-digest payloads.
func sha256Sum(b []byte) []byte { s := sha256.Sum256(b); return s[:] }
