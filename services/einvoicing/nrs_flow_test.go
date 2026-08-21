package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newNRSTestServer(t *testing.T) (*Server, *InprocWebhookSink, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewInvoiceStore(filepath.Join(dir, "invoices.jsonl"))
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
	sink := &InprocWebhookSink{}
	srv := &Server{
		store: store, outbox: outbox, signer: signer,
		validator: NewValidator(), router: NewAPPRouter(NewMBSClient()),
		runner: NewInprocRunner(), serviceIDs: NewServiceIDRegistry(),
		webhooks: NewWebhookRegistry(sink),
	}
	registerWorkflows(srv.runner)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/invoices/nrs", srv.handleNRSCreate)
	mux.HandleFunc("PATCH /v1/invoices/{irn}", srv.handleNRSUpdate)
	mux.HandleFunc("POST /v1/webhooks", srv.handleWebhookRegister)
	mux.HandleFunc("GET /v1/webhooks", srv.handleWebhookList)
	mux.HandleFunc("GET /v1/replay", srv.handleReplayList)
	return srv, sink, mux
}

func postNRS(t *testing.T, mux http.Handler, n NRSInvoice) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(n)
	req := httptest.NewRequest("POST", "/v1/invoices/nrs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

type nrsAPIResponse struct {
	IRN              string       `json:"irn"`
	Status           string       `json:"status"`
	InvoiceID        string       `json:"invoice_id"`
	RunID            string       `json:"run_id"`
	IdempotentReplay bool         `json:"idempotent_replay"`
	Steps            []Step       `json:"steps"`
	CryptoStamp      *CryptoStamp `json:"crypto_stamp"`
	QR               *struct {
		Payload string `json:"payload"`
		QRSVG   string `json:"qr_svg"`
	} `json:"qr"`
}

func TestNRSHappyPathEightSteps(t *testing.T) {
	_, _, mux := newNRSTestServer(t)
	rec := postNRS(t, mux, sampleNRSPayload())
	if rec.Code != 201 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp nrsAPIResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !ValidIRN(resp.IRN) {
		t.Fatalf("bad irn %q", resp.IRN)
	}
	num, sid, ds, _ := ParseIRN(resp.IRN)
	if num != "INV0001" || !ValidServiceID(sid) || ds != "20260127" {
		t.Fatalf("irn parts %q %q %q", num, sid, ds)
	}
	if resp.Status != "confirmed" {
		t.Fatalf("status=%s", resp.Status)
	}
	if len(resp.Steps) != 8 {
		t.Fatalf("steps=%d %+v", len(resp.Steps), resp.Steps)
	}
	wantOrder := []string{nrsStepCreate, nrsStepIRNGen, nrsStepIRNValid, nrsStepIRNSign,
		nrsStepSchema, nrsStepSign, nrsStepTransmit, nrsStepConfirm}
	for i, w := range wantOrder {
		if resp.Steps[i].Name != w || resp.Steps[i].Status != "ok" {
			t.Fatalf("step %d = %+v, want %s ok", i, resp.Steps[i], w)
		}
	}
	if resp.CryptoStamp == nil || resp.CryptoStamp.Signature == "" {
		t.Fatal("crypto stamp missing")
	}
	if resp.QR == nil || !strings.HasPrefix(resp.QR.Payload, "NRS1|") || resp.QR.QRSVG == "" {
		t.Fatal("qr missing")
	}
}

func TestNRSClientSuppliedIRNSkipsGeneration(t *testing.T) {
	_, _, mux := newNRSTestServer(t)
	n := sampleNRSPayload()
	n.IRN = "INV0001-94ND90NR-20260127"
	rec := postNRS(t, mux, n)
	if rec.Code != 201 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp nrsAPIResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.IRN != "INV0001-94ND90NR-20260127" {
		t.Fatalf("irn=%s", resp.IRN)
	}
	if !strings.Contains(resp.Steps[1].Detail, "skipped") {
		t.Fatalf("step2 detail=%q", resp.Steps[1].Detail)
	}
}

func TestNRSIdempotentResubmit(t *testing.T) {
	srv, _, mux := newNRSTestServer(t)
	n := sampleNRSPayload()
	n.IRN = "INV0001-94ND90NR-20260127"
	rec1 := postNRS(t, mux, n)
	if rec1.Code != 201 {
		t.Fatalf("first: %d %s", rec1.Code, rec1.Body)
	}
	// resubmission after success with the SAME IRN -> same invoice, no duplicate
	rec2 := postNRS(t, mux, n)
	if rec2.Code != 200 {
		t.Fatalf("second: %d %s", rec2.Code, rec2.Body)
	}
	var r1, r2 nrsAPIResponse
	_ = json.Unmarshal(rec1.Body.Bytes(), &r1)
	_ = json.Unmarshal(rec2.Body.Bytes(), &r2)
	if !r2.IdempotentReplay || r1.InvoiceID != r2.InvoiceID {
		t.Fatalf("idempotency broken: %+v vs %+v", r1, r2)
	}
	count := 0
	for _, inv := range srv.store.List() {
		if inv.IRN == n.IRN {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicate invoices for IRN: %d", count)
	}
}

func TestNRSSchemaFailureDescriptive(t *testing.T) {
	_, _, mux := newNRSTestServer(t)
	n := sampleNRSPayload()
	n.BusinessID = ""
	n.InvoiceTypeCode = "999"
	n.AccountingSupplierParty.PostalAddress.State = "NG-XX"
	rec := postNRS(t, mux, n)
	if rec.Code != 422 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var prob struct {
		Title  string     `json:"title"`
		Errors []NRSError `json:"errors"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &prob)
	if len(prob.Errors) < 3 {
		t.Fatalf("errors=%+v", prob.Errors)
	}
	body := rec.Body.String()
	for _, want := range []string{"business_id", "invoice_type_code", "postal_address.state"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in %s", want, body)
		}
	}
}

func patch(t *testing.T, mux http.Handler, irn string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("PATCH", "/v1/invoices/"+irn, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func confirmedInvoice(t *testing.T, mux http.Handler) string {
	t.Helper()
	rec := postNRS(t, mux, sampleNRSPayload())
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var resp nrsAPIResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp.IRN
}

func TestNRSUpdatePaymentStatus(t *testing.T) {
	srv, _, mux := newNRSTestServer(t)
	irn := confirmedInvoice(t, mux)
	rec := patch(t, mux, irn, map[string]string{"payment_status": "PAID", "reference": "TRX-99123"})
	if rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	inv, ok := srv.store.GetByIRN(irn)
	if !ok {
		t.Fatal("invoice gone")
	}
	if inv.PaymentStatus != "PAID" || inv.PaymentReference != "TRX-99123" {
		t.Fatalf("inv: %+v", inv)
	}
	if len(inv.Audit) == 0 || inv.Audit[len(inv.Audit)-1].Action != "payment_status_update" {
		t.Fatalf("audit: %+v", inv.Audit)
	}
	// event emitted
	rows, err := srv.outbox.Rows()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Topic == "nrs.einvoice.payment_status.v1" {
			found = true
		}
	}
	if !found {
		t.Fatal("nrs.einvoice.payment_status.v1 not emitted")
	}
	// core fields untouched
	if inv.CoreHash() != inv.SignedCoreHash {
		t.Fatal("core hash changed by payment update")
	}
}

func TestNRSUpdateLockedField409(t *testing.T) {
	_, _, mux := newNRSTestServer(t)
	irn := confirmedInvoice(t, mux)
	for _, body := range []map[string]any{
		{"payment_status": "PAID", "payable_amount": 1},
		{"invoice_line": []any{}},
		{"issue_date": "2026-01-28"},
	} {
		rec := patch(t, mux, irn, body)
		if rec.Code != 409 {
			t.Fatalf("body=%v -> %d %s", body, rec.Code, rec.Body)
		}
	}
}

func TestNRSUpdateInvalidStatus(t *testing.T) {
	_, _, mux := newNRSTestServer(t)
	irn := confirmedInvoice(t, mux)
	rec := patch(t, mux, irn, map[string]string{"payment_status": "PARTIAL"})
	if rec.Code != 422 {
		t.Fatalf("code=%d %s", rec.Code, rec.Body)
	}
}

func TestNRSUpdateNotFound(t *testing.T) {
	_, _, mux := newNRSTestServer(t)
	rec := patch(t, mux, "NOPE-94ND90NR-20260127", map[string]string{"payment_status": "PAID"})
	if rec.Code != 404 {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestWebhookSignatureVerify(t *testing.T) {
	body := []byte(`{"event":"nrs.einvoice.transmitted.v1"}`)
	sig := SignWebhook("secret-123", body)
	if !VerifyWebhookSignature("secret-123", body, sig) {
		t.Fatal("valid signature rejected")
	}
	if VerifyWebhookSignature("secret-123", []byte(`{"event":"tampered"}`), sig) {
		t.Fatal("tampered body accepted")
	}
	if VerifyWebhookSignature("wrong", body, sig) {
		t.Fatal("wrong secret accepted")
	}
}

func TestWebhookDeliveryOnTransmit(t *testing.T) {
	srv, sink, mux := newNRSTestServer(t)
	if err := srv.webhooks.Register("biz-acme", "t1", "https://stakeholder.example/hook", "topsecret"); err != nil {
		t.Fatal(err)
	}
	irn := confirmedInvoice(t, mux)
	if len(sink.Bodies) != 1 {
		t.Fatalf("deliveries=%d", len(sink.Bodies))
	}
	hdr := sink.Headers[0]
	sig := hdr["X-Meridian-Signature"]
	if sig == "" || !VerifyWebhookSignature("topsecret", sink.Bodies[0], sig) {
		t.Fatal("signature header invalid")
	}
	if hdr["X-Meridian-Event"] != "nrs.einvoice.transmitted.v1" {
		t.Fatalf("event header=%q", hdr["X-Meridian-Event"])
	}
	if !strings.Contains(string(sink.Bodies[0]), irn) {
		t.Fatal("payload missing irn")
	}
	ds := srv.webhooks.Deliveries()
	if len(ds) != 1 || ds[0].Status != "delivered" || ds[0].Attempts != 1 {
		t.Fatalf("deliveries=%+v", ds)
	}
}

func TestWebhookRetryBackoff(t *testing.T) {
	sink := &InprocWebhookSink{Fail: errors.New("connection refused")}
	reg := NewWebhookRegistry(sink)
	if err := reg.Register("biz-1", "t1", "https://x.example/h", "s"); err != nil {
		t.Fatal(err)
	}
	err := reg.Notify(context.Background(), "biz-1", "test.v1", map[string]string{"k": "v"})
	if err == nil {
		t.Fatal("expected error when all endpoints fail")
	}
	ds := reg.Deliveries()
	if len(ds) != 1 || ds[0].Status != "failed" || ds[0].Attempts != 3 {
		t.Fatalf("deliveries=%+v", ds)
	}
}

func TestWebhookNoEndpointsTransmissionSucceeds(t *testing.T) {
	// no registered stakeholders: transmission step still ok (happy path
	// already covers this implicitly; make it explicit)
	_, sink, mux := newNRSTestServer(t)
	irn := confirmedInvoice(t, mux)
	if irn == "" {
		t.Fatal("no irn")
	}
	if len(sink.Bodies) != 0 {
		t.Fatal("unexpected deliveries")
	}
}

func TestWebhookRegisterValidation(t *testing.T) {
	reg := NewWebhookRegistry(&InprocWebhookSink{})
	if err := reg.Register("", "t1", "https://x", "s"); err == nil {
		t.Fatal("empty business accepted")
	}
	if err := reg.Register("b", "t1", "ftp://x", "s"); err == nil {
		t.Fatal("non-http url accepted")
	}
	if err := reg.Register("b", "t1", "https://x", "s"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register("b", "t1", "https://x", "s"); err == nil {
		t.Fatal("duplicate accepted")
	}
	t.Setenv("ENV", "production")
	if err := reg.Register("b2", "t1", "http://insecure", "0123456789abcdef"); err == nil {
		t.Fatal("http accepted in prod (fail-closed broken)")
	}
	if err := reg.Register("b2", "t1", "https://x", "short"); err == nil {
		t.Fatal("weak secret accepted in prod (fail-closed broken)")
	}
	if err := reg.Register("b2", "t1", "https://x", "0123456789abcdef"); err == nil {
		// A1-05: prod refuses callbacks whose host does not resolve (SSRF
		// fail-closed); "x" has no DNS record.
		t.Fatal("unresolvable webhook host accepted in prod (SSRF fail-closed broken)")
	}
}
