package provider

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func ctx() context.Context { return context.Background() }

// --- software provider: parity with existing keyx conventions --------------

func TestSoftwareSignVerifyRoundtrip(t *testing.T) {
	p, err := NewSoftware(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("canonical-invoice-hash")
	sig, err := p.Sign(ctx(), "csid", payload)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := p.PublicKey(ctx(), "csid")
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), payload, sig) {
		t.Fatal("software ed25519 signature did not verify")
	}
	if p.Mode() != "software" {
		t.Fatalf("mode = %q", p.Mode())
	}
}

// Parity: a key file written by the legacy keyx convention
// (<dir>/csid_ed25519.key raw private key) must be picked up, and a
// CSID_SEED_HEX-style env seed must derive the same key.
func TestSoftwareParityLegacyKeyFileAndSeed(t *testing.T) {
	dir := t.TempDir()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "csid_ed25519.key"), priv, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := NewSoftware(dir)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := p.PublicKey(ctx(), "csid")
	if err != nil {
		t.Fatal(err)
	}
	want := priv.Public().(ed25519.PublicKey)
	if hex.EncodeToString(pub) != hex.EncodeToString(want) {
		t.Fatal("legacy key file not loaded (parity break)")
	}

	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CSID2_SEED_HEX", hex.EncodeToString(seed))
	pub2, err := p.PublicKey(ctx(), "csid2")
	if err != nil {
		t.Fatal(err)
	}
	want2 := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if hex.EncodeToString(pub2) != hex.EncodeToString(want2) {
		t.Fatal("env seed derivation mismatch (parity break)")
	}
	if err := p.Rotate(ctx(), "csid2"); err == nil {
		t.Fatal("env-seeded key must refuse in-process rotation")
	}
}

func TestSoftwareHMACAndRotation(t *testing.T) {
	p, err := NewSoftware(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("QR_HMAC_KEY", "test-secret")
	payload := []byte("NRS1|IRN123|TIN|1000|20250101000000")
	sig, err := p.Sign(ctx(), "qr-hmac", payload)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("test-secret"))
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		t.Fatal("HMAC mismatch vs direct computation")
	}
	if _, err := p.PublicKey(ctx(), "qr-hmac"); !errors.Is(err, ErrSymmetricKey) {
		t.Fatalf("expected ErrSymmetricKey, got %v", err)
	}
	// Rotation of a persisted (non-env) ed25519 key changes the public key.
	before, _ := p.PublicKey(ctx(), "feed")
	if err := p.Rotate(ctx(), "feed"); err != nil {
		t.Fatal(err)
	}
	after, _ := p.PublicKey(ctx(), "feed")
	if hex.EncodeToString(before) == hex.EncodeToString(after) {
		t.Fatal("rotation did not change the key")
	}
}

// --- cloud-kms against httptest fake ----------------------------------------

type fakeKMSKey struct {
	priv ed25519.PrivateKey
	mac  []byte
	ver  int
}

type fakeKMS struct {
	mu   sync.Mutex
	keys map[string]*fakeKMSKey
}

func newFakeKMS() *fakeKMS { return &fakeKMS{keys: map[string]*fakeKMSKey{}} }

func (f *fakeKMS) key(keyID string, hmacKey bool) *fakeKMSKey {
	k, ok := f.keys[keyID]
	if !ok {
		k = &fakeKMSKey{ver: 1}
		if hmacKey {
			k.mac = make([]byte, 32)
			_, _ = rand.Read(k.mac)
		} else {
			_, k.priv, _ = ed25519.GenerateKey(rand.Reader)
		}
		f.keys[keyID] = k
	}
	return k
}

func (f *fakeKMS) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/keys/{id}/sign", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var req struct {
			PayloadB64 string `json:"payload_b64"`
			Algorithm  string `json:"algorithm"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		payload, _ := base64.StdEncoding.DecodeString(req.PayloadB64)
		keyID := r.PathValue("id")
		k := f.key(keyID, req.Algorithm == "hmac-sha256")
		var sig []byte
		if req.Algorithm == "hmac-sha256" {
			m := hmac.New(sha256.New, k.mac)
			m.Write(payload)
			sig = m.Sum(nil)
		} else {
			sig = ed25519.Sign(k.priv, payload)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"signature_b64": base64.StdEncoding.EncodeToString(sig),
			"key_version":   strconv.Itoa(k.ver),
		})
	})
	mux.HandleFunc("GET /v1/keys/{id}/public", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		k := f.key(r.PathValue("id"), false)
		pub := k.priv.Public().(ed25519.PublicKey)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"public_key_b64": base64.StdEncoding.EncodeToString(pub), "algorithm": "ed25519",
		})
	})
	mux.HandleFunc("POST /v1/keys/{id}/rotate", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		k := f.key(r.PathValue("id"), IsHMACKeyID(r.PathValue("id")))
		if IsHMACKeyID(r.PathValue("id")) {
			k.mac = make([]byte, 32)
			_, _ = rand.Read(k.mac)
		} else {
			_, k.priv, _ = ed25519.GenerateKey(rand.Reader)
		}
		k.ver++
		_ = json.NewEncoder(w).Encode(map[string]any{"key_version": k.ver})
	})
	return mux
}

func TestCloudKMSSignVerifyRoundtrip(t *testing.T) {
	fk := newFakeKMS()
	srv := httptest.NewServer(fk.handler())
	defer srv.Close()
	p, err := NewCloudKMS(CloudKMSConfig{BaseURL: srv.URL, Style: "aws", BearerToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("tcc-document-bytes")
	sig, err := p.Sign(ctx(), "tcc", payload)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := p.PublicKey(ctx(), "tcc")
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), payload, sig) {
		t.Fatal("cloud-kms signature did not verify")
	}
	// HMAC path roundtrip.
	macSig, err := p.Sign(ctx(), "qr-hmac", payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(macSig) != sha256.Size {
		t.Fatalf("HMAC length = %d", len(macSig))
	}
	if p.Mode() != "cloud-kms" {
		t.Fatalf("mode = %q", p.Mode())
	}
}

func TestCloudKMSRotation(t *testing.T) {
	fk := newFakeKMS()
	srv := httptest.NewServer(fk.handler())
	defer srv.Close()
	p, err := NewCloudKMS(CloudKMSConfig{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("receipt-bytes")
	sig1, _ := p.Sign(ctx(), "receipt", payload)
	pub1, _ := p.PublicKey(ctx(), "receipt")
	if !ed25519.Verify(ed25519.PublicKey(pub1), payload, sig1) {
		t.Fatal("pre-rotation verify failed")
	}
	if err := p.Rotate(ctx(), "receipt"); err != nil {
		t.Fatal(err)
	}
	pub2, _ := p.PublicKey(ctx(), "receipt")
	if hex.EncodeToString(pub1) == hex.EncodeToString(pub2) {
		t.Fatal("rotation did not change KMS key version")
	}
	// Old signature must no longer verify against the rotated key.
	if ed25519.Verify(ed25519.PublicKey(pub2), payload, sig1) {
		t.Fatal("stale signature still verifies after rotation")
	}
	sig2, _ := p.Sign(ctx(), "receipt", payload)
	if !ed25519.Verify(ed25519.PublicKey(pub2), payload, sig2) {
		t.Fatal("post-rotation verify failed")
	}
}

func TestCloudKMSFailClosed(t *testing.T) {
	// Unreachable endpoint: sign must fail (fail-closed), not fall back.
	p, err := NewCloudKMS(CloudKMSConfig{BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Sign(ctx(), "csid", []byte("x")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
	// Endpoint returning a bogus signature must be rejected by local verify.
	bogus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/public") {
			_, priv, _ := ed25519.GenerateKey(rand.Reader)
			pub := priv.Public().(ed25519.PublicKey)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"public_key_b64": base64.StdEncoding.EncodeToString(pub), "algorithm": "ed25519"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"signature_b64": base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))})
	}))
	defer bogus.Close()
	p2, err := NewCloudKMS(CloudKMSConfig{BaseURL: bogus.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p2.Sign(ctx(), "csid", []byte("x")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable on bad signature, got %v", err)
	}
}

// --- env selection / fail-closed wiring -------------------------------------

func TestNewFromEnvFailClosed(t *testing.T) {
	t.Setenv("KEY_PROVIDER", "hsm")
	t.Setenv("KEY_PKCS11_PLUGIN", "")
	if _, err := NewFromEnv(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("hsm without plugin must fail closed, got %v", err)
	}
	t.Setenv("KEY_PROVIDER", "cloud-kms")
	t.Setenv("KMS_BASE_URL", "")
	if _, err := NewFromEnv(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cloud-kms without base URL must fail closed, got %v", err)
	}
	t.Setenv("KEY_PROVIDER", "bogus")
	if _, err := NewFromEnv(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unknown provider must fail closed, got %v", err)
	}
	t.Setenv("KEY_PROVIDER", "")
	t.Setenv("KEY_DIR", t.TempDir())
	p, err := NewFromEnv()
	if err != nil || p.Mode() != "software" {
		t.Fatalf("default must be software, got %v/%v", p, err)
	}
}

// --- [SIM] soft-token --------------------------------------------------------

func TestSoftTokenSIM(t *testing.T) {
	tok := NewSoftToken()
	payload := []byte("soft-token-payload")
	sig, err := tok.Sign(ctx(), "csid", payload)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := tok.PublicKey(ctx(), "csid")
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), payload, sig) {
		t.Fatal("soft-token signature did not verify")
	}
	if err := tok.Rotate(ctx(), "csid"); err != nil {
		t.Fatal(err)
	}
	pub2, _ := tok.PublicKey(ctx(), "csid")
	if hex.EncodeToString(pub) == hex.EncodeToString(pub2) {
		t.Fatal("soft-token rotation did not change key")
	}
	if ed25519.Verify(ed25519.PublicKey(pub2), payload, sig) {
		t.Fatal("stale signature verifies after soft-token rotation")
	}
}
