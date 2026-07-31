// Package provider mirrors meridian-core-platform/packages/keyx/provider
// (vendored interface — do NOT import core's module; kept byte-identical
// except this header). It is the HSM/KMS-abstraction key provider for all
// authoritative signing in the Meridian platform (e-invoice CSID, tax
// clearance / attribution feeds, WORM evidence receipts, QR HMAC codes).
//
// The SignerProvider interface lets signing run on HSM-backed keys instead
// of software keys without call-site changes. Provider selection is
// env-driven (KEY_PROVIDER):
//
//	software   [REAL] software keys — existing file/env ed25519 + HMAC keys.
//	           Development default. Keys never leave the host filesystem.
//	hsm|pkcs11 [REAL] interface + [SIM] soft-token. CGO-free: real HSM wiring
//	           is via a documented plugin binary/exec protocol
//	           (KEY_PKCS11_PLUGIN, see pkcs11.go); an in-process [SIM]
//	           soft-token backs tests.
//	cloud-kms  [REAL] HTTP signing path (AWS KMS / Azure Key Vault /
//	           GCP Cloud KMS REST style), env-configured, with local
//	           signature verification after every remote sign.
//
// FAIL-CLOSED: when KEY_PROVIDER names a non-software provider that cannot
// be initialised, NewFromEnv returns an error — callers MUST refuse startup
// rather than silently falling back to software keys.
package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// SignerProvider is the key-provider abstraction. keyID is an opaque logical
// key name (e.g. "csid", "qr-hmac", "feed", "receipt"); the provider maps it
// to a slot/alias/key-ARN internally.
type SignerProvider interface {
	// Sign signs payload with keyID and returns the raw signature bytes.
	Sign(ctx context.Context, keyID string, payload []byte) ([]byte, error)
	// PublicKey returns the public verification key for keyID
	// (raw ed25519 public key). It returns ErrSymmetricKey for HMAC keys.
	PublicKey(ctx context.Context, keyID string) ([]byte, error)
	// Rotate rotates keyID (new key version becomes active for signing).
	Rotate(ctx context.Context, keyID string) error
	// Mode reports the provider mode ("software" | "pkcs11" | "cloud-kms").
	Mode() string
}

// ErrSymmetricKey is returned by PublicKey for symmetric (HMAC) keys.
var ErrSymmetricKey = errors.New("provider: key is symmetric (HMAC); no public key")

// ErrUnavailable marks provider-unavailable failures (fail-closed signal).
var ErrUnavailable = errors.New("provider: configured key provider unavailable")

// Config is the env-sourced provider configuration.
type Config struct {
	// Provider: software (default) | hsm | pkcs11 | cloud-kms.
	Provider string // KEY_PROVIDER
	// Software provider.
	KeyDir string // KEY_DIR (default: <os.TempDir>/meridian-keys)
	// PKCS#11 exec-plugin provider.
	PKCS11Plugin string // KEY_PKCS11_PLUGIN — path to plugin binary
	// Cloud-KMS REST provider.
	KMSBaseURL string // KMS_BASE_URL — e.g. https://kms.<region>.amazonaws.com
	KMSStyle   string // KMS_STYLE — aws | azure | gcp (request shapes)
	KMSToken   string // KMS_BEARER_TOKEN — static bearer token (dev/injection)
}

// ConfigFromEnv reads the provider configuration from the environment.
func ConfigFromEnv() Config {
	return Config{
		Provider:     strings.ToLower(strings.TrimSpace(os.Getenv("KEY_PROVIDER"))),
		KeyDir:       os.Getenv("KEY_DIR"),
		PKCS11Plugin: os.Getenv("KEY_PKCS11_PLUGIN"),
		KMSBaseURL:   os.Getenv("KMS_BASE_URL"),
		KMSStyle:     strings.ToLower(strings.TrimSpace(os.Getenv("KMS_STYLE"))),
		KMSToken:     os.Getenv("KMS_BEARER_TOKEN"),
	}
}

// NewFromEnv builds the configured SignerProvider. Empty/"software" selects
// the dev software provider. A configured but unavailable non-software
// provider is a hard error (fail-closed; no silent software fallback).
func NewFromEnv() (SignerProvider, error) { return New(ConfigFromEnv()) }

// New builds a SignerProvider from an explicit Config.
func New(cfg Config) (SignerProvider, error) {
	switch cfg.Provider {
	case "", "software":
		return NewSoftware(cfg.KeyDir)
	case "hsm", "pkcs11":
		if cfg.PKCS11Plugin == "" {
			return nil, fmt.Errorf("%w: KEY_PROVIDER=%s requires KEY_PKCS11_PLUGIN (exec protocol, see pkcs11.go)", ErrUnavailable, cfg.Provider)
		}
		p, err := NewPKCS11Plugin(cfg.PKCS11Plugin)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return p, nil
	case "cloud-kms", "kms":
		if cfg.KMSBaseURL == "" {
			return nil, fmt.Errorf("%w: KEY_PROVIDER=%s requires KMS_BASE_URL", ErrUnavailable, cfg.Provider)
		}
		return NewCloudKMS(CloudKMSConfig{
			BaseURL: cfg.KMSBaseURL, Style: cfg.KMSStyle, BearerToken: cfg.KMSToken,
		})
	default:
		return nil, fmt.Errorf("%w: unknown KEY_PROVIDER %q (software|hsm|pkcs11|cloud-kms)", ErrUnavailable, cfg.Provider)
	}
}
