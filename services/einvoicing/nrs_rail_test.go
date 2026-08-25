package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
)

// Feature I1: the endpoint map must mirror the nactp NRS/Gention v1.1
// catalog (services/nrs-client/firs/nrs_client.py NRS_ENDPOINTS) exactly.
func TestNRSEndpointCatalogMatchesGention(t *testing.T) {
	want := map[string]string{
		"AuthLogin":     "/api/v1/auth/login",
		"InvoiceUpload": "/api/v1/invoice/upload",
		"InvoiceUpdate": "/api/v1/invoice/update/{irn}",
		"InvoiceStatus": "/api/v1/invoice/{irn}",
		"InvoiceList":   "/api/v1/invoice/list",
		"TaxCategories": "/api/v1/resources/tax-categories",
		"InvoiceTypes":  "/api/v1/resources/invoice-types",
		"PaymentMeans":  "/api/v1/resources/payment-means",
		"ServiceCodes":  "/api/v1/resources/service-codes",
		"ProductCodes":  "/api/v1/resources/product-codes",
		"LGAs":          "/api/v1/resources/lgas",
		"Currencies":    "/api/v1/resources/currencies",
		"States":        "/api/v1/resources/states",
		"Countries":     "/api/v1/resources/countries",
	}
	e := NRSEndpointCatalog()
	got := map[string]string{
		"AuthLogin": e.AuthLogin, "InvoiceUpload": e.InvoiceUpload,
		"InvoiceUpdate": e.InvoiceUpdate, "InvoiceStatus": e.InvoiceStatus,
		"InvoiceList": e.InvoiceList, "TaxCategories": e.TaxCategories,
		"InvoiceTypes": e.InvoiceTypes, "PaymentMeans": e.PaymentMeans,
		"ServiceCodes": e.ServiceCodes, "ProductCodes": e.ProductCodes,
		"LGAs": e.LGAs, "Currencies": e.Currencies,
		"States": e.States, "Countries": e.Countries,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("endpoint %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestNRSEndpointPathTemplating(t *testing.T) {
	e := NRSEndpointCatalog()
	if got := e.InvoiceUpdatePath("INV-1-ABCDEFGH-20260101"); got != "/api/v1/invoice/update/INV-1-ABCDEFGH-20260101" {
		t.Errorf("update path = %q", got)
	}
	if got := e.InvoiceStatusPath("IRNX"); got != "/api/v1/invoice/IRNX" {
		t.Errorf("status path = %q", got)
	}
}

// Profile selection: MBS_PROFILE=live with full config selects the live rail.
func TestMBSProfileLiveSelectsLiveRail(t *testing.T) {
	t.Setenv("MBS_PROFILE", "live")
	t.Setenv("MBS_LIVE_BASE_URL", "https://api.einvoice.firs.gov.ng")
	t.Setenv("MBS_LIVE_API_KEY", "k")
	t.Setenv("MBS_LIVE_API_SECRET", "s")
	t.Setenv("MBS_LIVE_EMAIL", "e@x.test")
	t.Setenv("MBS_LIVE_PASSWORD", "p")
	c := NewMBSClient()
	if _, ok := c.(*LiveRailClient); !ok {
		t.Fatalf("live profile client = %T, want *LiveRailClient", c)
	}
}

// Explicit sandbox profile keeps the simulator in dev.
func TestMBSProfileSandboxSelectsSandbox(t *testing.T) {
	t.Setenv("PROFILE", "")
	t.Setenv("MBS_PROFILE", "sandbox")
	t.Setenv("MBS_BASE_URL", "")
	if _, ok := NewMBSClient().(*SandboxMBS); !ok {
		t.Fatalf("sandbox profile client = %T, want *SandboxMBS", NewMBSClient())
	}
}

// Fail-closed boot: helper-process pattern (same as mbs_prod_test.go).
func fatalHelper(t *testing.T, name string, env ...string) {
	t.Helper()
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		NewMBSClient()
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run="+name)
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	cmd.Env = append(cmd.Env, env...)
	if err := cmd.Run(); err == nil {
		t.Fatalf("expected non-zero exit for env %v", env)
	}
}

// PROFILE=prod + MBS_PROFILE=live + missing credentials must hard-fail.
func TestMBSProdLiveMissingCredsFatals(t *testing.T) {
	fatalHelper(t, "TestMBSProdLiveMissingCredsFatals",
		"PROFILE=prod", "MBS_PROFILE=live",
		"MBS_LIVE_BASE_URL=", "MBS_LIVE_API_KEY=", "MBS_LIVE_API_SECRET=",
		"MBS_LIVE_EMAIL=", "MBS_LIVE_PASSWORD=",
		"NRS_BASE_URL=", "NRS_API_KEY=", "NRS_API_SECRET=", "NRS_EMAIL=", "NRS_PASSWORD=")
}

// PROFILE=prod + MBS_PROFILE=sandbox must hard-fail.
func TestMBSProdSandboxProfileFatals(t *testing.T) {
	fatalHelper(t, "TestMBSProdSandboxProfileFatals", "PROFILE=prod", "MBS_PROFILE=sandbox")
}

// Unknown MBS_PROFILE value must hard-fail (fail-closed).
func TestMBSUnknownProfileFatals(t *testing.T) {
	fatalHelper(t, "TestMBSUnknownProfileFatals", "MBS_PROFILE=bogus")
}

// Live profile without prod but missing creds must also hard-fail.
func TestMBSDevLiveMissingCredsFatals(t *testing.T) {
	fatalHelper(t, "TestMBSDevLiveMissingCredsFatals",
		"PROFILE=", "MBS_PROFILE=live",
		"MBS_LIVE_BASE_URL=", "MBS_LIVE_API_KEY=", "MBS_LIVE_API_SECRET=",
		"MBS_LIVE_EMAIL=", "MBS_LIVE_PASSWORD=",
		"NRS_BASE_URL=", "NRS_API_KEY=", "NRS_API_SECRET=", "NRS_EMAIL=", "NRS_PASSWORD=")
}

// Auth token flow: login posts to /api/v1/auth/login, then the upload call
// carries x-api-key, x-api-secret and Bearer JWT per the Gention catalog.
func TestLiveRailAuthAndUploadFlow(t *testing.T) {
	var sawLogin, sawUpload bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			sawLogin = true
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["email"] != "e@x.test" || body["password"] != "p" {
				t.Errorf("login body = %v", body)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]string{"access_token": "jwt-123"},
			})
		case "/api/v1/invoice/upload":
			sawUpload = true
			if got := r.Header.Get("x-api-key"); got != "k" {
				t.Errorf("x-api-key = %q", got)
			}
			if got := r.Header.Get("x-api-secret"); got != "s" {
				t.Errorf("x-api-secret = %q", got)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer jwt-123" {
				t.Errorf("Authorization = %q", got)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"irn": "INV-1-ABCDEFGH-20260101", "status": "cleared"},
			})
		default:
			t.Errorf("unexpected rail path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewLiveRailClient(LiveRailConfig{
		BaseURL: srv.URL, APIKey: "k", APISecret: "s",
		Email: "e@x.test", Password: "p",
	})
	res, err := c.Preclear(context.Background(), &CanonicalInvoice{InvoiceNumber: "INV-1"}, []byte("<xml/>"))
	if err != nil {
		t.Fatalf("Preclear: %v", err)
	}
	if !sawLogin || !sawUpload {
		t.Fatalf("login=%v upload=%v, want both", sawLogin, sawUpload)
	}
	if res.IRN != "INV-1-ABCDEFGH-20260101" || res.Status != "cleared" {
		t.Errorf("result = %+v", res)
	}
}
