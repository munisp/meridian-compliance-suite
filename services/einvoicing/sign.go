package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/munisp/meridian-compliance-suite/packages/keyx/provider"
)

// CSIDSigner is the Cryptographic Stamp / Signing Identity module (SPEC §3
// T1/T2): supplier-side ed25519 signing. Dev keys are generated once and
// persisted (or seeded deterministically via CSID_SEED_HEX).
type CSIDSigner struct {
	priv  ed25519.PrivateKey
	pub   ed25519.PublicKey
	keyID string
	// prov, when non-nil and non-software, routes signing to the HSM/KMS key
	// provider (KEY_PROVIDER=hsm|pkcs11|cloud-kms); priv stays nil and key
	// material never leaves the provider.
	prov provider.SignerProvider
}

// LoadCSIDWithProvider loads the CSID signer through the key-provider
// abstraction. A nil or software-mode prov keeps the existing dev file/env
// key behaviour (LoadCSID). A non-software prov (HSM/KMS) signs remotely;
// the provider is expected to have been constructed fail-closed already —
// this constructor additionally fails closed if the provider cannot serve
// the "csid" public key.
func LoadCSIDWithProvider(dir string, prov provider.SignerProvider) (*CSIDSigner, error) {
	if prov == nil || prov.Mode() == "software" {
		return LoadCSID(dir)
	}
	pub, err := prov.PublicKey(context.Background(), "csid")
	if err != nil {
		return nil, fmt.Errorf("csid: provider public key: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("csid: provider returned %d-byte public key, want ed25519", len(pub))
	}
	pk := ed25519.PublicKey(append([]byte(nil), pub...))
	return &CSIDSigner{prov: prov, pub: pk, keyID: shortKeyID(pk)}, nil
}

// LoadCSID loads or creates the dev signing keypair in dir.
func LoadCSID(dir string) (*CSIDSigner, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, "csid_ed25519.key")
	if seedHex := os.Getenv("CSID_SEED_HEX"); seedHex != "" {
		seed, err := hex.DecodeString(seedHex)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("CSID_SEED_HEX must be 32-byte hex")
		}
		priv := ed25519.NewKeyFromSeed(seed)
		return &CSIDSigner{priv: priv, pub: priv.Public().(ed25519.PublicKey), keyID: shortKeyID(priv.Public().(ed25519.PublicKey))}, nil
	}
	if data, err := os.ReadFile(keyPath); err == nil && len(data) == ed25519.PrivateKeySize {
		priv := ed25519.PrivateKey(data)
		return &CSIDSigner{priv: priv, pub: priv.Public().(ed25519.PublicKey), keyID: shortKeyID(priv.Public().(ed25519.PublicKey))}, nil
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, priv, 0o600); err != nil {
		return nil, err
	}
	return &CSIDSigner{priv: priv, pub: pub, keyID: shortKeyID(pub)}, nil
}

func shortKeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "csid-" + hex.EncodeToString(sum[:4])
}

// KeyID returns the public key identifier.
func (s *CSIDSigner) KeyID() string { return s.keyID }

// PublicKeyHex exposes the dev public key for verification tooling.
func (s *CSIDSigner) PublicKeyHex() string { return hex.EncodeToString(s.pub) }

// SignPayload signs an arbitrary payload, returning hex signature.
// Dev-software convenience wrapper; provider-backed callers should use
// SignPayloadE so signing errors propagate (fail-closed).
func (s *CSIDSigner) SignPayload(payload string) string {
	sig, _ := s.SignPayloadE(payload)
	return sig
}

// SignPayloadE signs an arbitrary payload, returning hex signature; errors
// from a provider-backed (HSM/KMS) signer are returned to the caller.
func (s *CSIDSigner) SignPayloadE(payload string) (string, error) {
	if s.prov != nil {
		sig, err := s.prov.Sign(context.Background(), "csid", []byte(payload))
		if err != nil {
			return "", fmt.Errorf("csid sign: %w", err)
		}
		return hex.EncodeToString(sig), nil
	}
	sig := ed25519.Sign(s.priv, []byte(payload))
	return hex.EncodeToString(sig), nil
}

// SignInvoice signs the canonical invoice hash and records it on the invoice.
func (s *CSIDSigner) SignInvoice(inv *CanonicalInvoice) error {
	sig, err := s.SignPayloadE(inv.Hash())
	if err != nil {
		return err
	}
	inv.CSIDSignature = sig
	inv.CSIDKeyID = s.keyID
	return nil
}

// Verify checks a hex signature over a payload against a hex public key.
func Verify(pubHex, payload, sigHex string) bool {
	pub, err1 := hex.DecodeString(pubHex)
	sig, err2 := hex.DecodeString(sigHex)
	if err1 != nil || err2 != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), []byte(payload), sig)
}
