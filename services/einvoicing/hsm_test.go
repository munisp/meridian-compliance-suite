package main

// Tests for the HSM/KMS key-provider wiring of CSID + QR signing
// (KEY_PROVIDER=hsm|pkcs11|cloud-kms). The [SIM] soft-token stands in for
// the HSM; fail-closed startup behaviour is asserted directly.

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/munisp/meridian-compliance-suite/packages/keyx/provider"
)

func TestCSIDProviderBackedSignAndVerify(t *testing.T) {
	tok := provider.NewSoftToken() // [SIM] HSM soft-token
	signer, err := LoadCSIDWithProvider(t.TempDir(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if signer.priv != nil {
		t.Fatal("provider-backed signer must not hold private key material")
	}
	inv := sampleInvoice()
	if err := signer.SignInvoice(inv); err != nil {
		t.Fatal(err)
	}
	if inv.CSIDSignature == "" || inv.CSIDKeyID == "" {
		t.Fatal("invoice not signed")
	}
	if !Verify(signer.PublicKeyHex(), inv.Hash(), inv.CSIDSignature) {
		t.Fatal("provider-backed CSID signature did not verify")
	}
	// Same key served by the provider's PublicKey endpoint.
	pub, err := tok.PublicKey(context.Background(), "csid")
	if err != nil {
		t.Fatal(err)
	}
	if signer.PublicKeyHex() != hex.EncodeToString(pub) {
		t.Fatal("signer public key differs from provider public key")
	}
}

func TestCSIDSoftwareProviderParity(t *testing.T) {
	// Explicit software provider must behave exactly like legacy LoadCSID.
	prov, err := provider.NewSoftware(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s1, err := LoadCSIDWithProvider(dir, prov)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := LoadCSID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s1.PublicKeyHex() != s2.PublicKeyHex() {
		t.Fatal("software-provider path diverged from legacy LoadCSID")
	}
}

func TestQRProviderBackedSignAndVerify(t *testing.T) {
	tok := provider.NewSoftToken() // [SIM]
	inv := sampleInvoice()
	inv.IRN = "IRN-TEST-1"
	payload, sig, err := QRPayloadE(tok, inv)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := VerifyQRPayloadE(tok, payload, sig)
	if err != nil || !ok {
		t.Fatalf("provider-backed QR verify failed: ok=%v err=%v", ok, err)
	}
	if ok, _ := VerifyQRPayloadE(tok, payload, "deadbeef0000"); ok {
		t.Fatal("bogus QR signature accepted")
	}
	// Provider-backed QR signature must differ from the dev software key
	// signature (different key material), proving routing occurred.
	_, devSig := QRPayload(inv)
	if devSig == sig {
		t.Fatal("provider-backed QR signature identical to dev-key signature — not routed")
	}
}

func TestKeyProviderFailClosedStartup(t *testing.T) {
	// KEY_PROVIDER=hsm with no plugin binary must refuse startup.
	t.Setenv("KEY_PROVIDER", "hsm")
	t.Setenv("KEY_PKCS11_PLUGIN", "")
	if _, err := provider.NewFromEnv(); !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("expected fail-closed ErrUnavailable, got %v", err)
	}
	t.Setenv("KEY_PKCS11_PLUGIN", "/nonexistent/pkcs11-plugin")
	if _, err := provider.NewFromEnv(); !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("expected fail-closed for missing plugin binary, got %v", err)
	}
	// Unknown provider must also fail closed.
	t.Setenv("KEY_PROVIDER", "tpm-magic")
	if _, err := provider.NewFromEnv(); !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("expected fail-closed for unknown provider, got %v", err)
	}
	// Dev default remains software.
	t.Setenv("KEY_PROVIDER", "")
	t.Setenv("KEY_DIR", t.TempDir())
	p, err := provider.NewFromEnv()
	if err != nil || p.Mode() != "software" {
		t.Fatalf("dev default must be software: %v %v", p, err)
	}
}

func TestCSIDProviderUnavailableFailsClosed(t *testing.T) {
	// A provider that cannot serve the csid public key fails construction.
	p, err := provider.NewCloudKMS(provider.CloudKMSConfig{BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCSIDWithProvider(t.TempDir(), p); err == nil {
		t.Fatal("expected LoadCSIDWithProvider to fail closed against unreachable KMS")
	}
}
