package provider

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Software is the [REAL] software-key provider (development default). It
// preserves the existing keyx conventions: ed25519 keypairs persisted as raw
// private-key files under KEY_DIR, or seeded deterministically via
// <KEYID>_SEED_HEX (e.g. CSID_SEED_HEX). HMAC keys (keyID suffix "-hmac")
// use a secret from <KEYID>_KEY env (e.g. QR_HMAC_KEY) or a generated
// persisted secret.
type Software struct {
	dir  string
	mu   sync.Mutex
	keys map[string]ed25519.PrivateKey // asymmetric keys by keyID
	macs map[string][]byte             // symmetric keys by keyID
}

// NewSoftware opens (creating if needed) the software key store in dir.
func NewSoftware(dir string) (*Software, error) {
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "meridian-keys")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Software{dir: dir, keys: map[string]ed25519.PrivateKey{}, macs: map[string][]byte{}}, nil
}

// Mode implements SignerProvider.
func (s *Software) Mode() string { return "software" }

// IsHMACKeyID reports whether keyID names a symmetric (HMAC-SHA256) key.
func IsHMACKeyID(keyID string) bool { return strings.HasSuffix(keyID, "-hmac") }

func envName(keyID, suffix string) string {
	n := strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(keyID))
	return n + suffix
}

func (s *Software) ed25519Key(keyID string) (ed25519.PrivateKey, error) {
	if k, ok := s.keys[keyID]; ok {
		return k, nil
	}
	if seedHex := os.Getenv(envName(keyID, "_SEED_HEX")); seedHex != "" {
		seed, err := hex.DecodeString(seedHex)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("%s must be 32-byte hex", envName(keyID, "_SEED_HEX"))
		}
		priv := ed25519.NewKeyFromSeed(seed)
		s.keys[keyID] = priv
		return priv, nil
	}
	keyPath := filepath.Join(s.dir, keyID+"_ed25519.key")
	if data, err := os.ReadFile(keyPath); err == nil && len(data) == ed25519.PrivateKeySize {
		priv := ed25519.PrivateKey(data)
		s.keys[keyID] = priv
		return priv, nil
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, priv, 0o600); err != nil {
		return nil, err
	}
	s.keys[keyID] = priv
	return priv, nil
}

func (s *Software) hmacKey(keyID string) ([]byte, error) {
	if k, ok := s.macs[keyID]; ok {
		return k, nil
	}
	if v := os.Getenv(envName(keyID, "_KEY")); v != "" {
		s.macs[keyID] = []byte(v)
		return s.macs[keyID], nil
	}
	keyPath := filepath.Join(s.dir, keyID+".key")
	if data, err := os.ReadFile(keyPath); err == nil && len(data) >= 32 {
		s.macs[keyID] = data
		return data, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return nil, err
	}
	s.macs[keyID] = key
	return key, nil
}

// Sign implements SignerProvider: ed25519 for asymmetric keys,
// HMAC-SHA256 for "-hmac" keys.
func (s *Software) Sign(_ context.Context, keyID string, payload []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if IsHMACKeyID(keyID) {
		key, err := s.hmacKey(keyID)
		if err != nil {
			return nil, err
		}
		mac := hmac.New(sha256.New, key)
		mac.Write(payload)
		return mac.Sum(nil), nil
	}
	priv, err := s.ed25519Key(keyID)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(priv, payload), nil
}

// PublicKey implements SignerProvider.
func (s *Software) PublicKey(_ context.Context, keyID string) ([]byte, error) {
	if IsHMACKeyID(keyID) {
		return nil, ErrSymmetricKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	priv, err := s.ed25519Key(keyID)
	if err != nil {
		return nil, err
	}
	pub := priv.Public().(ed25519.PublicKey)
	out := make([]byte, len(pub))
	copy(out, pub)
	return out, nil
}

// Rotate implements SignerProvider: generates a fresh ed25519 keypair (or
// HMAC secret) and atomically replaces the persisted key. Env-seeded keys
// cannot be rotated in-process.
func (s *Software) Rotate(_ context.Context, keyID string) error {
	if os.Getenv(envName(keyID, "_SEED_HEX")) != "" || os.Getenv(envName(keyID, "_KEY")) != "" {
		return fmt.Errorf("provider: key %q is env-seeded; rotate by updating the environment", keyID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if IsHMACKeyID(keyID) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(s.dir, keyID+".key"), key, 0o600); err != nil {
			return err
		}
		s.macs[keyID] = key
		return nil
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.dir, keyID+"_ed25519.key"), priv, 0o600); err != nil {
		return err
	}
	s.keys[keyID] = priv
	return nil
}
