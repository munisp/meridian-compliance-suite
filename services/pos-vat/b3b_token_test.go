package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// B3 #5 regression: the pos-vat HTTP ledger client must authenticate to
// the core ledger with the env-injected shared service token
// (X-Service-Token) and only fall back to the forgeable X-Dev-Role when
// no token is configured (dev only).

func captureLedgerHeaders(t *testing.T, fn func(base string)) http.Header {
	t.Helper()
	got := make(http.Header)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"t1"}`))
	}))
	defer srv.Close()
	fn(srv.URL)
	return got
}

func TestLedgerClientSendsServiceToken(t *testing.T) {
	t.Setenv("MERIDIAN_SERVICE_TOKEN", "svc-tok-123")
	got := captureLedgerHeaders(t, func(base string) {
		h := &HTTPLedger{Base: base}
		if _, err := h.Transfer(LedgerTransfer{}); err != nil {
			t.Fatalf("transfer: %v", err)
		}
	})
	if got.Get("X-Service-Token") != "svc-tok-123" {
		t.Fatalf("X-Service-Token = %q, want svc-tok-123", got.Get("X-Service-Token"))
	}
	if got.Get("X-Service-Name") != "pos-vat" {
		t.Fatalf("X-Service-Name = %q", got.Get("X-Service-Name"))
	}
	if got.Get("X-Dev-Role") != "" {
		t.Fatal("X-Dev-Role sent while a service token is configured")
	}
}

func TestLedgerClientServiceTokenFallbackEnv(t *testing.T) {
	t.Setenv("LEDGER_SERVICE_TOKEN", "ledger-tok")
	got := captureLedgerHeaders(t, func(base string) {
		h := &HTTPLedger{Base: base}
		if _, err := h.Transfer(LedgerTransfer{}); err != nil {
			t.Fatalf("transfer: %v", err)
		}
	})
	if got.Get("X-Service-Token") != "ledger-tok" {
		t.Fatalf("LEDGER_SERVICE_TOKEN fallback not honoured: %q", got.Get("X-Service-Token"))
	}
}

func TestLedgerClientDevRoleOnlyWithoutToken(t *testing.T) {
	got := captureLedgerHeaders(t, func(base string) {
		h := &HTTPLedger{Base: base}
		if _, err := h.Transfer(LedgerTransfer{}); err != nil {
			t.Fatalf("transfer: %v", err)
		}
	})
	if got.Get("X-Service-Token") != "" {
		t.Fatal("X-Service-Token sent with none configured")
	}
	if got.Get("X-Dev-Role") != "operator" {
		t.Fatalf("dev fallback X-Dev-Role = %q", got.Get("X-Dev-Role"))
	}
}
