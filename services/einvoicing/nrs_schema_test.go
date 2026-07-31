package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleNRSPayload() NRSInvoice {
	return NRSInvoice{
		BusinessID:           "biz-acme",
		IssueDate:            "2026-01-27",
		DueDate:              "2026-02-27",
		InvoiceTypeCode:      "380",
		DocumentCurrencyCode: "NGN",
		InvoiceKind:          "B2B",
		PaymentStatus:        "PENDING",
		BuyerReference:       "INV0001",
		AccountingSupplierParty: NRSParty{
			PartyName: "Acme Supplies Ltd", TIN: "1234567890123",
			Email: "ap@acme.ng", Telephone: "+2348000000001",
			PostalAddress: NRSPostalAddress{
				StreetName: "1 Marina Rd", CityName: "Lagos",
				Country: "NG", State: "NG-LA", LGA: "NG-LA-IKE",
			},
		},
		AccountingCustomerParty: NRSParty{
			PartyName: "Buyer Co", TIN: "9876543210987",
			PostalAddress: NRSPostalAddress{CityName: "Aba", Country: "NG", State: "NG-AB", LGA: "NG-AB-ANO"},
		},
		InvoiceLine: []NRSInvoiceLine{{
			HSNCode: "2523", InvoicedQuantity: 10, LineExtensionAmount: 85000.00,
			Item:  NRSItem{Name: "Cement 50kg", Description: "Portland cement"},
			Price: NRSPrice{PriceAmount: 8500.00, BaseQuantity: 1},
		}},
		TaxTotal: []NRSTaxTotal{{
			TaxAmount: 6375.00,
			TaxSubtotal: []NRSTaxSubtotal{{
				TaxableAmount: 85000.00, TaxAmount: 6375.00,
				TaxCategory: NRSTaxCategory{ID: "STANDARD_VAT", Percent: 7.5},
			}},
		}},
		PaymentMeans: []NRSPaymentMeans{{PaymentMeansCode: "42", PaymentDueDate: "2026-02-27"}},
		LegalMonetaryTotal: NRSLegalMonetaryTotal{
			LineExtensionAmount: 85000.00, TaxExclusiveAmount: 85000.00,
			TaxInclusiveAmount: 91375.00, PayableAmount: 91375.00,
		},
	}
}

func TestNGNToKoboRounding(t *testing.T) {
	cases := []struct {
		ngn  float64
		kobo int64
	}{
		{0, 0},
		{0.5, 50},
		{2.5, 250},
		{10.10, 1010},
		{85000.00, 8500000},
		{91375.00, 9137500},
		// float64 noise from decimal serialisation must still round correctly
		{245236.28024999998, 24523628},
		{0.575, 58},          // 57.5 -> half up 58
		{99.99999999, 10000}, // within a kobo of 100.00
		{-245236.28024999998, -24523628},
	}
	for _, c := range cases {
		if got := NGNToKobo(c.ngn); got != c.kobo {
			t.Fatalf("NGNToKobo(%v) = %d, want %d", c.ngn, got, c.kobo)
		}
	}
}

func TestFromNRSHappyPath(t *testing.T) {
	n := sampleNRSPayload()
	inv, err := FromNRS(&n)
	if err != nil {
		t.Fatal(err)
	}
	if inv.BusinessID != "biz-acme" || inv.InvoiceNumber != "INV0001" {
		t.Fatalf("bad mapping: %+v", inv)
	}
	if inv.TaxExclusiveKobo != 8500000 || inv.TaxKobo != 637500 || inv.PayableKobo != 9137500 {
		t.Fatalf("totals: excl=%d tax=%d pay=%d", inv.TaxExclusiveKobo, inv.TaxKobo, inv.PayableKobo)
	}
	if len(inv.Lines) != 1 {
		t.Fatal("lines missing")
	}
	l := inv.Lines[0]
	if l.QuantityMilli != 10000 || l.UnitPriceKobo != 850000 || l.LineTotalKobo != 8500000 {
		t.Fatalf("line: %+v", l)
	}
	if l.VatCategory != "S" || l.VatRateBps != 750 {
		t.Fatalf("STANDARD_VAT not mapped to S/750: %+v", l)
	}
	if inv.Supplier.TIN != "1234567890123" || inv.Supplier.State != "NG-LA" {
		t.Fatalf("supplier: %+v", inv.Supplier)
	}
	if inv.PaymentStatus != "PENDING" || inv.InvoiceTypeCode != "380" {
		t.Fatalf("status/type: %+v", inv)
	}
}

func TestFromNRSInvoiceNumberFromIRN(t *testing.T) {
	n := sampleNRSPayload()
	n.BuyerReference = ""
	n.IRN = "INV0007-94ND90NR-20260127"
	inv, err := FromNRS(&n)
	if err != nil {
		t.Fatal(err)
	}
	if inv.InvoiceNumber != "INV0007" || inv.IRN != "INV0007-94ND90NR-20260127" {
		t.Fatalf("number=%q irn=%q", inv.InvoiceNumber, inv.IRN)
	}
}

func TestFromNRSValidationErrorList(t *testing.T) {
	// field-order-independent JSON with multiple violations; every violation
	// must appear in the NRS-style error list.
	raw := `{
		"payment_status": "SOMEDAY",
		"document_currency_code": "XXX",
		"invoice_type_code": "999",
		"issue_date": "2026-01-27",
		"accounting_supplier_party": {"party_name": "", "tin": "",
			"postal_address": {"state": "NG-XX", "lga": "bogus"}},
		"accounting_customer_party": {"party_name": "Buyer"},
		"invoice_line": [{"item": {"name": ""}, "invoiced_quantity": -1,
			"line_extension_amount": 0, "price": {"price_amount": -5}}],
		"tax_total": [{"tax_amount": 0, "tax_subtotal": [{"taxable_amount":0,"tax_amount":0,
			"tax_category": {"id": "MADE_UP", "percent": 3}}]}],
		"payment_means": [{"payment_means_code": "77"}],
		"legal_monetary_total": {"payable_amount": -1}
	}`
	var n NRSInvoice
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatal(err)
	}
	_, err := FromNRS(&n)
	errs, ok := err.(NRSErrors)
	if !ok {
		t.Fatalf("expected NRSErrors, got %v", err)
	}
	msg := errs.Error()
	for _, want := range []string{
		"business_id", "invoice_type_code", "document_currency_code", "payment_status",
		"accounting_supplier_party.party_name", "accounting_supplier_party.tin",
		"postal_address.state", "postal_address.lga",
		"invoice_line[0].item.name", "invoiced_quantity", "price_amount",
		"tax_category.id", "payment_means_code", "payable_amount",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error list missing %q:\n%s", want, msg)
		}
	}
	if len(errs) < 12 {
		t.Fatalf("expected >=12 violations, got %d", len(errs))
	}
}

func TestFromNRSFloatPrecision(t *testing.T) {
	n := sampleNRSPayload()
	n.BuyerReference = ""
	n.IRN = "PREC0001-94ND90NR-20260127"
	n.InvoiceLine[0].LineExtensionAmount = 245236.28024999998
	n.TaxTotal = nil                               // derive tax from category default
	n.LegalMonetaryTotal = NRSLegalMonetaryTotal{} // derive all totals
	inv, err := FromNRS(&n)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Lines[0].LineTotalKobo != 24523628 {
		t.Fatalf("line kobo=%d", inv.Lines[0].LineTotalKobo)
	}
	if inv.TaxExclusiveKobo != 24523628 {
		t.Fatalf("excl=%d", inv.TaxExclusiveKobo)
	}
	wantTax := RoundBpsHalfUp(24523628, 750)
	if inv.TaxKobo != wantTax || inv.PayableKobo != 24523628+wantTax {
		t.Fatalf("tax=%d pay=%d", inv.TaxKobo, inv.PayableKobo)
	}
}

func TestFromNRSDiscountFeeLineMath(t *testing.T) {
	n := sampleNRSPayload()
	n.InvoiceLine[0].LineExtensionAmount = 0 // force derivation
	n.InvoiceLine[0].DiscountAmount = 500.00
	n.InvoiceLine[0].FeeAmount = 250.00
	inv, err := FromNRS(&n)
	if err != nil {
		t.Fatal(err)
	}
	// gross 10 * 8500.00 = 85000.00 - 500.00 + 250.00 = 84750.00 -> 8_475_000 kobo
	if inv.Lines[0].LineTotalKobo != 8475000 {
		t.Fatalf("line=%d", inv.Lines[0].LineTotalKobo)
	}
}

func TestToNRSRoundTrip(t *testing.T) {
	n := sampleNRSPayload()
	inv, err := FromNRS(&n)
	if err != nil {
		t.Fatal(err)
	}
	inv.IRN = "INV0001-94ND90NR-20260127"
	out := ToNRS(inv)
	if out.IRN != inv.IRN || out.PaymentStatus != "PENDING" {
		t.Fatalf("out: %+v", out)
	}
	if out.LegalMonetaryTotal.PayableAmount != 91375.00 {
		t.Fatalf("payable=%v", out.LegalMonetaryTotal.PayableAmount)
	}
	if out.TaxTotal[0].TaxSubtotal[0].TaxCategory.ID != "STANDARD_VAT" {
		t.Fatalf("tax cat: %+v", out.TaxTotal[0])
	}
	if out.InvoiceLine[0].LineExtensionAmount != 85000.00 {
		t.Fatalf("line ext=%v", out.InvoiceLine[0].LineExtensionAmount)
	}
	// round trip back to kobo
	inv2, err := FromNRS(out)
	if err != nil {
		t.Fatal(err)
	}
	if inv2.PayableKobo != inv.PayableKobo {
		t.Fatalf("round trip payable %d != %d", inv2.PayableKobo, inv.PayableKobo)
	}
}

func TestNRSValidatePaymentStatusEnum(t *testing.T) {
	n := sampleNRSPayload()
	n.PaymentStatus = "REJECTED"
	if errs := n.Validate(); len(errs) != 0 {
		t.Fatalf("unexpected: %v", errs)
	}
	n.PaymentStatus = "partial"
	if errs := n.Validate(); len(errs) == 0 {
		t.Fatal("bad payment_status accepted")
	}
}

func TestFromNRSRequiresInvoiceNumberSource(t *testing.T) {
	n := sampleNRSPayload()
	n.BuyerReference = ""
	n.OrderReference = ""
	if _, err := FromNRS(&n); err == nil {
		t.Fatal("expected error when no invoice number source")
	}
}
