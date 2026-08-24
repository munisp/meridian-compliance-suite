package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// Regression: B4-4 — PATCH /v1/invoices/{irn} skipped the tenant check when
// the caller's tenant_id claim was EMPTY (inverted polarity), letting any
// authenticated principal flip another tenant's invoice to PAID.
// B4-13 — GET /v1/webhooks returned ALL tenants' delivery history.

func createSignedInvoiceWithIRN(t *testing.T, s *Server, tenant, irn string) string {
	t.Helper()
	id := createInvoice(t, s, tenant)
	inv, ok := s.store.Get(id)
	if !ok {
		t.Fatalf("invoice %s not stored", id)
	}
	inv.IRN = irn
	if _, err := s.store.Save(inv); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestBOLAUpdateByIRNCrossTenant(t *testing.T) {
	s := newBOLAServer(t)
	createSignedInvoiceWithIRN(t, s, "tenant-a", "IRN-A-0001")
	body := []byte(`{"payment_status":"PAID","reference":"forge"}`)

	// cross-tenant: 404 (no existence oracle), invoice untouched
	if rec := doReq(s.handleNRSUpdate, "PATCH", "/v1/invoices/IRN-A-0001", "tenant-b", body, "irn", "IRN-A-0001"); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant PATCH: want 404, got %d (%s)", rec.Code, rec.Body.String())
	}
	// empty tenant claim: 403, never a void check
	if rec := doReq(s.handleNRSUpdate, "PATCH", "/v1/invoices/IRN-A-0001", "", body, "irn", "IRN-A-0001"); rec.Code != http.StatusForbidden {
		t.Fatalf("empty-tenant PATCH: want 403, got %d (%s)", rec.Code, rec.Body.String())
	}
	inv, _ := s.store.GetByIRN("IRN-A-0001")
	if inv.PaymentStatus == "PAID" {
		t.Fatal("cross-tenant/empty-claim PATCH mutated payment_status")
	}
	// owning tenant may update
	if rec := doReq(s.handleNRSUpdate, "PATCH", "/v1/invoices/IRN-A-0001", "tenant-a", body, "irn", "IRN-A-0001"); rec.Code != http.StatusOK {
		t.Fatalf("same-tenant PATCH: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	inv, _ = s.store.GetByIRN("IRN-A-0001")
	if inv.PaymentStatus != "PAID" {
		t.Fatalf("same-tenant PATCH did not apply: %+v", inv)
	}
}

func TestWebhookListTenantScopedDeliveries(t *testing.T) {
	s := newBOLAServer(t)
	s.webhooks = NewWebhookRegistry(&InprocWebhookSink{})
	regBody := []byte(`{"business_id":"biz-a","url":"https://a.example/hook","secret":"supersecretvalue123"}`)
	if rec := doReq(s.handleWebhookRegister, "POST", "/v1/webhooks", "tenant-a", regBody); rec.Code != http.StatusCreated {
		t.Fatalf("register: want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	if err := s.webhooks.Notify(context.Background(), "biz-a", "invoice.signed.v1", map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}

	// tenant-a sees its delivery
	rec := doReq(s.handleWebhookList, "GET", "/v1/webhooks?business_id=biz-a", "tenant-a", nil)
	if rec.Code != 200 {
		t.Fatalf("tenant-a list: %d (%s)", rec.Code, rec.Body.String())
	}
	var outA struct {
		Deliveries []WebhookDelivery `json:"deliveries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &outA); err != nil {
		t.Fatal(err)
	}
	if len(outA.Deliveries) != 1 {
		t.Fatalf("tenant-a deliveries=%+v, want 1", outA.Deliveries)
	}

	// tenant-b (unscoped list) must NOT see tenant-a's delivery history
	rec = doReq(s.handleWebhookList, "GET", "/v1/webhooks", "tenant-b", nil)
	if rec.Code != 200 {
		t.Fatalf("tenant-b list: %d (%s)", rec.Code, rec.Body.String())
	}
	var outB struct {
		Deliveries []WebhookDelivery `json:"deliveries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &outB); err != nil {
		t.Fatal(err)
	}
	if len(outB.Deliveries) != 0 {
		t.Fatalf("tenant-b saw %d cross-tenant deliveries: %+v", len(outB.Deliveries), outB.Deliveries)
	}

	// empty tenant claim: 403 even without business_id
	if rec := doReq(s.handleWebhookList, "GET", "/v1/webhooks", "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("empty-tenant list: want 403, got %d", rec.Code)
	}
}
