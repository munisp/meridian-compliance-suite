package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/munisp/meridian-compliance-suite/packages/shared/devjwt"
)

// newBOLAServer builds a Server sufficient for the tenant-isolation tests.
func newBOLAServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	store, err := NewInvoiceStore(dir + "/invoices.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := newTestOutbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := LoadCSID(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		store: store, outbox: outbox, signer: signer,
		validator: NewValidator(),
		router:    NewAPPRouter(NewMBSClient()),
		runner:    NewInprocRunner(),
	}
	registerWorkflows(s.runner)
	return s
}

// doReq wraps h in the dev auth middleware and issues a request carrying the
// given tenant (empty tenant == service token without a tenant_id claim).
// pathVals are {id}-style path values for handlers that use PathValue.
func doReq(h http.HandlerFunc, method, path, tenant string, body []byte, pathVals ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("X-Dev-Role", "operator")
	if tenant != "" {
		req.Header.Set("X-Tenant-ID", tenant)
	}
	for i := 0; i+1 < len(pathVals); i += 2 {
		req.SetPathValue(pathVals[i], pathVals[i+1])
	}
	rec := httptest.NewRecorder()
	devjwt.Middleware(h).ServeHTTP(rec, req)
	return rec
}

func createInvoice(t *testing.T, s *Server, tenant string) string {
	t.Helper()
	inv := sampleInvoice()
	raw, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	rec := doReq(s.handleCreateInvoice, "POST", "/v1/invoices", tenant, raw)
	if rec.Code != 201 {
		t.Fatalf("create: want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Invoices []CanonicalInvoice `json:"invoices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Invoices) == 0 {
		t.Fatalf("create: bad response %v %s", err, rec.Body.String())
	}
	return out.Invoices[0].ID
}

// TestBOLAGetInvoiceCrossTenant (audit fix H-3): tenant B cannot read tenant
// A's invoice (404, no existence oracle); an empty tenant_id claim is a 403,
// never a void check.
func TestBOLAGetInvoiceCrossTenant(t *testing.T) {
	s := newBOLAServer(t)
	id := createInvoice(t, s, "tenant-a")

	if rec := doReq(s.handleGetInvoice, "GET", "/v1/invoices/"+id, "tenant-a", nil, "id", id); rec.Code != 200 {
		t.Fatalf("same tenant: want 200, got %d", rec.Code)
	}
	if rec := doReq(s.handleGetInvoice, "GET", "/v1/invoices/"+id, "tenant-b", nil, "id", id); rec.Code != 404 {
		t.Fatalf("cross tenant: want 404, got %d", rec.Code)
	}
	if rec := doReq(s.handleGetInvoice, "GET", "/v1/invoices/"+id, "", nil, "id", id); rec.Code != 403 {
		t.Fatalf("empty tenant claim: want 403, got %d", rec.Code)
	}
	// QR endpoint follows the same rules
	if rec := doReq(s.handleGetQR, "GET", "/v1/invoices/"+id+"/qr", "tenant-b", nil, "id", id); rec.Code != 404 {
		t.Fatalf("qr cross tenant: want 404, got %d", rec.Code)
	}
	if rec := doReq(s.handleGetQR, "GET", "/v1/invoices/"+id+"/qr", "", nil, "id", id); rec.Code != 403 {
		t.Fatalf("qr empty tenant claim: want 403, got %d", rec.Code)
	}
}

// TestBOLAPreclearCrossTenant: tenant B cannot trigger tenant A's
// pre-clearance workflow.
func TestBOLAPreclearCrossTenant(t *testing.T) {
	s := newBOLAServer(t)
	id := createInvoice(t, s, "tenant-a")

	if rec := doReq(s.handlePreclear, "POST", "/v1/invoices/"+id+"/preclear", "tenant-b", nil, "id", id); rec.Code != 404 {
		t.Fatalf("cross tenant preclear: want 404, got %d", rec.Code)
	}
	if rec := doReq(s.handlePreclear, "POST", "/v1/invoices/"+id+"/preclear", "", nil, "id", id); rec.Code != 403 {
		t.Fatalf("empty tenant preclear: want 403, got %d", rec.Code)
	}
	inv, _ := s.store.Get(id)
	if inv.Status == "cleared" || inv.IRN != "" {
		t.Fatalf("cross-tenant preclear mutated the invoice: %+v", inv)
	}
	if rec := doReq(s.handlePreclear, "POST", "/v1/invoices/"+id+"/preclear", "tenant-a", nil, "id", id); rec.Code != 200 {
		t.Fatalf("same tenant preclear: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestBOLAB2CReportCrossTenant: tenant B cannot report against tenant A's
// stored invoice.
func TestBOLAB2CReportCrossTenant(t *testing.T) {
	s := newBOLAServer(t)
	id := createInvoice(t, s, "tenant-a")

	body, _ := json.Marshal(map[string]string{"invoice_id": id})
	if rec := doReq(s.handleB2CReport, "POST", "/v1/b2c/report", "tenant-b", body); rec.Code != 404 {
		t.Fatalf("cross tenant b2c: want 404, got %d", rec.Code)
	}
	if rec := doReq(s.handleB2CReport, "POST", "/v1/b2c/report", "", body); rec.Code != 403 {
		t.Fatalf("empty tenant b2c: want 403, got %d", rec.Code)
	}
	inv, _ := s.store.Get(id)
	if inv.Status == "reported" {
		t.Fatal("cross-tenant b2c mutated the invoice")
	}
}

// TestQRKeyFailClosed (audit fix M-4): prod mode without an explicit
// QR_HMAC_KEY refuses to start; the silent dev-key default is dev-only.
func TestQRKeyFailClosed(t *testing.T) {
	t.Setenv("AUTH_MODE", "keycloak")
	t.Setenv("QR_HMAC_KEY", "")
	if err := validateQRKey(); err == nil {
		t.Fatal("want error in keycloak mode without QR_HMAC_KEY")
	}
	t.Setenv("QR_HMAC_KEY", "prod-key-from-kms")
	if err := validateQRKey(); err != nil {
		t.Fatalf("explicit key must pass: %v", err)
	}
	t.Setenv("AUTH_MODE", "dev")
	t.Setenv("QR_HMAC_KEY", "")
	if err := validateQRKey(); err != nil {
		t.Fatalf("dev mode keeps the documented dev default: %v", err)
	}
}
