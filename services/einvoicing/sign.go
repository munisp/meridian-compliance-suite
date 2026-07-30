package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// CSIDSigner is the Cryptographic Stamp / Signing Identity module (SPEC §3
// T1/T2): supplier-side ed25519 signing. Dev keys are generated once and
// persisted (or seeded deterministically via CSID_SEED_HEX).
type CSIDSigner struct {
	priv  ed25519.PrivateKey
	pub   ed25519.PublicKey
	keyID string
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
func (s *CSIDSigner) SignPayload(payload string) string {
	sig := ed25519.Sign(s.priv, []byte(payload))
	return hex.EncodeToString(sig)
}

// SignInvoice signs the canonical invoice hash and records it on the invoice.
func (s *CSIDSigner) SignInvoice(inv *CanonicalInvoice) {
	inv.CSIDSignature = s.SignPayload(inv.Hash())
	inv.CSIDKeyID = s.keyID
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
