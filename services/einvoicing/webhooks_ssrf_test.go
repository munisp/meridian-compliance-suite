package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/munisp/meridian-compliance-suite/packages/shared/devjwt"
)

// TestWebhookRegisterBOLA (A1-05 regression): an authenticated tenant must
// NOT be able to register a webhook callback for another tenant's
// business_id. Pre-fix: any principal could register any URL for any
// business (201). Post-fix: 403.
func TestWebhookRegisterBOLA(t *testing.T) {
	s := newBOLAServer(t)
	s.webhooks = NewWebhookRegistry(&InprocWebhookSink{})
	id := createInvoice(t, s, "tenant-a")
	inv, _ := s.store.Get(id)
	if inv.BusinessID == "" {
		inv.BusinessID = "biz-a1"
		if _, err := s.store.Save(inv); err != nil {
			t.Fatal(err)
		}
	}
	body, _ := json.Marshal(map[string]any{
		"business_id": inv.BusinessID,
		"url":         "https://stakeholder.example/hook",
		"secret":      "0123456789abcdef",
	})

	// owning tenant can register
	if rec := doReq(s.handleWebhookRegister, "POST", "/v1/webhooks", "tenant-a", body); rec.Code != 201 {
		t.Fatalf("owner register: want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	// different tenant must be denied (distinct URL: pre-fix this returned
	// 201 and registered the attacker's callback for tenant-a's business)
	evilBody, _ := json.Marshal(map[string]any{
		"business_id": inv.BusinessID,
		"url":         "https://evil.example/exfil",
		"secret":      "0123456789abcdef",
	})
	if rec := doReq(s.handleWebhookRegister, "POST", "/v1/webhooks", "tenant-b", evilBody); rec.Code != 403 {
		t.Fatalf("cross-tenant register: want 403, got %d (%s)", rec.Code, rec.Body.String())
	}
	for _, ep := range s.webhooks.Endpoints(inv.BusinessID) {
		if strings.Contains(ep.URL, "evil.example") {
			t.Fatalf("cross-tenant registration mutated registry: %+v", ep)
		}
	}
	// cross-tenant list is denied too
	if rec := doReq(s.handleWebhookList, "GET", "/v1/webhooks?business_id="+inv.BusinessID, "tenant-b", nil); rec.Code != 403 {
		t.Fatalf("cross-tenant list: want 403, got %d", rec.Code)
	}
}

// TestWebhookRegisterRoleGate (A1-05): non-admin/operator roles are denied.
func TestWebhookRegisterRoleGate(t *testing.T) {
	s := newBOLAServer(t)
	body, _ := json.Marshal(map[string]any{
		"business_id": "biz-x", "url": "https://cb.example/h", "secret": "0123456789abcdef",
	})
	req := doReqRaw(s.handleWebhookRegister, "POST", "/v1/webhooks", "tenant-a", "auditor", body)
	if req.Code != 403 {
		t.Fatalf("auditor role: want 403, got %d", req.Code)
	}
}

// TestWebhookSSRFURLPolicy (A1-05 regression): outside dev, callback URLs
// resolving to loopback/RFC1918/link-local are refused; non-http(s) schemes
// always refused; allowlist enforced when configured.
func TestWebhookSSRFURLPolicy(t *testing.T) {
	// scheme policy applies in dev too
	for _, u := range []string{"ftp://x.example/h", "file:///etc/passwd", "gopher://x/"} {
		if err := validateWebhookURL(u); err == nil {
			t.Fatalf("scheme %s accepted", u)
		}
	}
	// prod: private destinations refused
	t.Setenv("PROFILE", "prod")
	for _, u := range []string{
		"https://127.0.0.1/hook", "https://localhost/hook",
		"https://10.0.0.4/hook", "https://192.168.1.1/hook",
		"https://169.254.169.254/latest/meta-data", // cloud metadata
	} {
		if err := validateWebhookURL(u); err == nil {
			t.Fatalf("prod accepted private/loopback target %s", u)
		}
	}
	if err := validateWebhookURL("http://callback.example.com/h"); err == nil {
		t.Fatal("prod accepted plain http callback")
	}
	// allowlist
	t.Setenv("WEBHOOK_URL_ALLOWLIST", "example.com")
	if err := validateWebhookURL("https://evil.attacker.io/h"); err == nil {
		t.Fatal("allowlist bypass: non-matching host accepted")
	}
	t.Setenv("WEBHOOK_URL_ALLOWLIST", "")
}

func doReqRaw(h http.HandlerFunc, method, path, tenant, role string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("X-Dev-Role", role)
	if tenant != "" {
		req.Header.Set("X-Tenant-ID", tenant)
	}
	rec := httptest.NewRecorder()
	devjwt.Middleware(h).ServeHTTP(rec, req)
	return rec
}
