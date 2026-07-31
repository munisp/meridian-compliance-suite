package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func sampleInvoice() *CanonicalInvoice {
	inv := &CanonicalInvoice{
		InvoiceNumber: "INV-2026-0001",
		InvoiceType:   "B2B",
		IssueDate:     "2026-01-15",
		Supplier:      Party{TIN: "1234567890123", Name: "Acme Supplies Ltd", State: "Lagos"},
		Customer:      Party{TIN: "9876543210987", Name: "Buyer Co"},
		Lines: []InvoiceLine{
			{Description: "Cement 50kg", QuantityMilli: 10000, UnitPriceKobo: 850000, VatCategory: "S", VatRateBps: 750},
			{Description: "Delivery", QuantityMilli: 1000, UnitPriceKobo: 500000, VatCategory: "S", VatRateBps: 750},
		},
	}
	inv.Normalise()
	return inv
}

func TestNormaliseTotals(t *testing.T) {
	inv := sampleInvoice()
	// 10 * 8500.00 = 85000.00 -> 8_500_000 kobo; 1 * 5000.00 = 500_000 kobo
	if inv.TaxExclusiveKobo != 9000000 {
		t.Fatalf("excl=%d", inv.TaxExclusiveKobo)
	}
	if inv.TaxKobo != 675000 { // 7.5%
		t.Fatalf("tax=%d", inv.TaxKobo)
	}
	if inv.PayableKobo != 9675000 {
		t.Fatalf("payable=%d", inv.PayableKobo)
	}
	if !inv.TotalsConsistent() {
		t.Fatal("totals inconsistent")
	}
}

func TestUBLGeneration(t *testing.T) {
	inv := sampleInvoice()
	out, err := GenerateUBL(inv)
	if err != nil {
		t.Fatalf("ubl: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"urn:oasis:names:specification:ubl:schema:xsd:Invoice-2",
		"<cbc:ID>INV-2026-0001</cbc:ID>",
		"1234567890123",
		"<cbc:PayableAmount currencyID=\"NGN\">96750.00</cbc:PayableAmount>",
		"urn:fdc:peppol.eu:2017:poacc:billing:3.0",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("UBL missing %q\n%s", want, s)
		}
	}
	// must be well-formed XML
	var doc any
	if err := xml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("ubl not well-formed: %v", err)
	}
}

func TestCSIDSignVerify(t *testing.T) {
	dir := t.TempDir()
	signer, err := LoadCSID(dir)
	if err != nil {
		t.Fatal(err)
	}
	inv := sampleInvoice()
	signer.SignInvoice(inv)
	if inv.CSIDSignature == "" || inv.CSIDKeyID == "" {
		t.Fatal("not signed")
	}
	if !Verify(signer.PublicKeyHex(), inv.Hash(), inv.CSIDSignature) {
		t.Fatal("signature does not verify")
	}
	// key reload persistence
	signer2, _ := LoadCSID(dir)
	if signer2.PublicKeyHex() != signer.PublicKeyHex() {
		t.Fatal("key not persisted")
	}
}

func TestMBSSandboxPreclear(t *testing.T) {
	mbs := NewSandboxMBS()
	inv := sampleInvoice()
	dir := t.TempDir()
	signer, _ := LoadCSID(dir)
	signer.SignInvoice(inv)
	res, err := mbs.Preclear(context.Background(), inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "cleared" || res.IRN == "" || res.Stamp == nil {
		t.Fatalf("bad result %+v", res)
	}
	if !strings.HasPrefix(res.IRN, "IRN-1234-20260115-") {
		t.Fatalf("irn=%s", res.IRN)
	}
	// stamp verifies against MBS public key
	if !Verify(mbs.PublicKeyHex(), res.Stamp.Payload, res.Stamp.Signature) {
		t.Fatal("MBS stamp does not verify")
	}
	// unsigned invoice rejected
	inv2 := sampleInvoice()
	res2, _ := mbs.Preclear(context.Background(), inv2, nil)
	if res2.Status != "rejected" {
		t.Fatal("unsigned invoice should be rejected")
	}
}

func TestValidatorPacks(t *testing.T) {
	v := NewValidator()
	inv := sampleInvoice()
	violations, fatal, err := v.Validate(inv, false)
	if err != nil {
		t.Fatal(err)
	}
	if fatal {
		t.Fatalf("unexpected fatal: %+v", violations)
	}
	// missing TIN → fatal
	bad := sampleInvoice()
	bad.Supplier.TIN = ""
	v2, fatal2, _ := v.Validate(bad, false)
	if !fatal2 {
		t.Fatalf("expected fatal for missing TIN, got %+v", v2)
	}
}

func newTestServer(t *testing.T) *Server {
	dir := filepath.Join(t.TempDir(), "data")
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
	srv := &Server{
		store: store, outbox: outbox, signer: signer,
		validator: NewValidator(), router: NewAPPRouter(NewMBSClient()),
		runner: NewInprocRunner(),
	}
	registerWorkflows(srv.runner)
	return srv
}

func TestEndToEndPreclear(t *testing.T) {
	srv := newTestServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/invoices", srv.handleCreateInvoice)
	mux.HandleFunc("GET /v1/invoices/{id}", srv.handleGetInvoice)
	mux.HandleFunc("POST /v1/invoices/{id}/preclear", srv.handlePreclear)
	mux.HandleFunc("POST /v1/b2c/report", srv.handleB2CReport)
	mux.HandleFunc("GET /v1/replay", srv.handleReplayList)

	si := sampleInvoice()
	si.ID = "" // ERP payloads carry no internal id
	body, _ := json.Marshal(si)
	req := httptest.NewRequest("POST", "/v1/invoices", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "test-key-1")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		Invoices []*CanonicalInvoice `json:"invoices"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created.Invoices[0].ID

	// idempotent replay returns prior invoice
	req2 := httptest.NewRequest("POST", "/v1/invoices", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Idempotency-Key", "test-key-1")
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if !strings.Contains(rec2.Body.String(), "idempotent_replay") {
		t.Fatalf("expected idempotent replay: code=%d body=%s", rec2.Code, rec2.Body)
	}

	// preclear
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, httptest.NewRequest("POST", "/v1/invoices/"+id+"/preclear", nil))
	if rec3.Code != 200 {
		t.Fatalf("preclear: %d %s", rec3.Code, rec3.Body)
	}
	var pr struct {
		Invoice *CanonicalInvoice `json:"invoice"`
	}
	_ = json.Unmarshal(rec3.Body.Bytes(), &pr)
	if pr.Invoice.IRN == "" || pr.Invoice.Stamp == nil || pr.Invoice.Status != "precleared" {
		t.Fatalf("not precleared: %+v", pr.Invoice)
	}

	// get with UBL
	req4 := httptest.NewRequest("GET", "/v1/invoices/"+id+"?format=ubl", nil)
	rec4 := httptest.NewRecorder()
	mux.ServeHTTP(rec4, req4)
	if !strings.Contains(rec4.Body.String(), "ubl:Invoice") {
		t.Fatalf("ubl not returned: %s", rec4.Body.String()[:200])
	}

	// B2C realtime report
	rec5 := httptest.NewRecorder()
	b2c, _ := json.Marshal(map[string]string{"invoice_id": id})
	mux.ServeHTTP(rec5, httptest.NewRequest("POST", "/v1/b2c/report", bytes.NewReader(b2c)))
	if rec5.Code != 200 || !strings.Contains(rec5.Body.String(), "B2C-RPT-") {
		t.Fatalf("b2c: %d %s", rec5.Code, rec5.Body)
	}

	// replay queue has events
	rec6 := httptest.NewRecorder()
	mux.ServeHTTP(rec6, httptest.NewRequest("GET", "/v1/replay", nil))
	var rl struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec6.Body.Bytes(), &rl)
	if rl.Count < 2 {
		t.Fatalf("expected outbox events, got %d", rl.Count)
	}
}

func TestCSVAdapter(t *testing.T) {
	csvBody := "invoice_number,supplier_tin,supplier_name,line_description,quantity_milli,unit_price_kobo,vat_category,vat_rate_bps\n" +
		"INV-9,1234567890123,Acme,Widget,2000,150000,S,750\n" +
		"INV-9,1234567890123,Acme,Gadget,1000,250000,S,750\n"
	invs, err := CSVAdapter{}.Parse([]byte(csvBody))
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 || len(invs[0].Lines) != 2 {
		t.Fatalf("parse: %+v", invs)
	}
	invs[0].Normalise()
	if invs[0].TaxExclusiveKobo != 550000 {
		t.Fatalf("excl=%d", invs[0].TaxExclusiveKobo)
	}
}

func TestSAPODataAdapter(t *testing.T) {
	payload := `{"value":[{"DocNum":"SAP-100","DocDate":"2026-02-01T00:00:00Z","DocCurrency":"NGN",
	  "CardName":"Buyer Ltd","U_CustomerTIN":"9876543210987","U_SupplierTIN":"1234567890123","U_SupplierName":"Acme",
	  "DocumentLines":[{"ItemCode":"A1","ItemDescription":"Bolt M8","Quantity":100,"UnitPriceKobo":5000,"TaxPercent":7.5}]}]}`
	invs, err := SAPODataAdapter{}.Parse([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 || invs[0].InvoiceNumber != "SAP-100" {
		t.Fatalf("parse: %+v", invs)
	}
	invs[0].Normalise()
	if invs[0].TaxExclusiveKobo != 500000 || invs[0].IssueDate != "2026-02-01" {
		t.Fatalf("map: %+v", invs[0])
	}
}

func TestMultiAPPRouter(t *testing.T) {
	r := NewAPPRouter(NewSandboxMBS())
	app, err := r.Resolve("any-tenant")
	if err != nil || app.ID != "mbs-sandbox" {
		t.Fatalf("resolve: %v %+v", err, app)
	}
	r.Register(&APP{ID: "app-beta", Client: NewSandboxMBS(), Kind: "simulator"})
	r.Route("tenant-x", "app-beta")
	app2, err := r.Resolve("tenant-x")
	if err != nil || app2.ID != "app-beta" {
		t.Fatalf("route: %v %+v", err, app2)
	}
}

// TestPortalPayloadContract replays the EXACT JSON the compliance-portal
// Einvoicing page posts to /v1/invoices (portals/compliance-portal/src/pages/
// Einvoicing.tsx) through RESTAdapter.Parse + Normalise + Validator. Guards the
// audit finding where supplier_tin/buyer_tin/qty were silently dropped.
func TestPortalPayloadContract(t *testing.T) {
	portalPayload := `{
	  "invoice_number": "WEB-1785000000000",
	  "invoice_type": "B2B",
	  "issue_date": "2026-07-31",
	  "currency": "NGN",
	  "supplier": {"tin": "12345678-0001", "name": "Supplier 12345678-0001", "country": "NG"},
	  "customer": {"tin": "87654321-0001", "name": "Buyer 87654321-0001", "country": "NG"},
	  "lines": [{"id": "1", "description": "Consulting services",
	    "quantity_milli": 1000, "unit_price_kobo": 10000000,
	    "vat_category": "S", "vat_rate_bps": 750}]
	}`
	invs, err := RESTAdapter{}.Parse([]byte(portalPayload))
	if err != nil {
		t.Fatalf("portal payload must parse: %v", err)
	}
	if len(invs) != 1 {
		t.Fatalf("expected 1 invoice, got %d", len(invs))
	}
	inv := invs[0]
	if inv.Supplier.TIN != "12345678-0001" || inv.Customer.TIN != "87654321-0001" {
		t.Fatalf("party TINs dropped: %+v / %+v", inv.Supplier, inv.Customer)
	}
	inv.Normalise()
	if inv.Lines[0].LineTotalKobo != 10000000 {
		t.Fatalf("line total not computed from quantity_milli: %+v", inv.Lines[0])
	}
	if inv.PayableKobo != 10750000 { // N100,000 + 7.5% VAT
		t.Fatalf("payable wrong: %d", inv.PayableKobo)
	}
	v := NewValidator()
	violations, fatal, err := v.Validate(inv, false)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if fatal {
		t.Fatalf("portal invoice must pass validation, violations: %+v", violations)
	}
}

// TestLegacyPortalShapeFailsLoud documents the OLD broken portal shape: unknown
// fields are dropped and validation must fail fatally (not silently clear).
func TestLegacyPortalShapeFailsLoud(t *testing.T) {
	legacy := `{"supplier_tin": "12345678-0001", "buyer_tin": "87654321-0001",
	  "lines": [{"description": "Consulting services", "qty": 1, "unit_price_kobo": 10000000}],
	  "currency": "NGN"}`
	invs, err := RESTAdapter{}.Parse([]byte(legacy))
	if err != nil || len(invs) != 1 {
		t.Fatalf("parse: %v", err)
	}
	inv := invs[0]
	inv.Normalise()
	v := NewValidator()
	_, fatal, err := v.Validate(inv, false)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !fatal {
		t.Fatalf("legacy shape must fail validation (missing TINs/zero totals)")
	}
}
