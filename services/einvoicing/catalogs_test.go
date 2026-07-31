package main

import "testing"

func TestTaxCategoryCatalog(t *testing.T) {
	c, ok := ValidTaxCategory("STANDARD_VAT")
	if !ok || c.RateBps != 750 || c.Canonical != "S" {
		t.Fatalf("STANDARD_VAT -> %+v ok=%v", c, ok)
	}
	for _, id := range []string{"ZERO_VAT", "EXEMPT"} {
		c, ok := ValidTaxCategory(id)
		if !ok || c.RateBps != 0 {
			t.Fatalf("%s -> %+v ok=%v", id, c, ok)
		}
	}
	if _, ok := ValidTaxCategory("NOPE"); ok {
		t.Fatal("unknown tax category accepted")
	}
	// case-insensitive
	if _, ok := ValidTaxCategory("standard_vat"); !ok {
		t.Fatal("case-insensitive lookup failed")
	}
}

func TestInvoiceTypeCodeCatalog(t *testing.T) {
	for _, code := range []string{"380", "381", "383"} {
		if !ValidInvoiceTypeCode(code) {
			t.Fatalf("invoice type %s rejected", code)
		}
	}
	if ValidInvoiceTypeCode("999") {
		t.Fatal("unknown invoice type accepted")
	}
}

func TestPaymentMeansCatalog(t *testing.T) {
	if !ValidPaymentMeansCode("10") || !ValidPaymentMeansCode("42") {
		t.Fatal("known payment means rejected")
	}
	if ValidPaymentMeansCode("77") {
		t.Fatal("unknown payment means accepted")
	}
}

func TestStateCodeCatalog(t *testing.T) {
	if len(StateCodes) != 37 { // 36 states + FCT
		t.Fatalf("state count=%d", len(StateCodes))
	}
	if !ValidStateCode("NG-AB") || !ValidStateCode("NG-LA") || !ValidStateCode("NG-FC") {
		t.Fatal("known states rejected")
	}
	if ValidStateCode("NG-XX") || ValidStateCode("AB") {
		t.Fatal("unknown state accepted")
	}
}

func TestLGACodeCatalog(t *testing.T) {
	if !ValidLGACode("NG-AB-ANO") { // spec example
		t.Fatal("NG-AB-ANO rejected")
	}
	if !ValidLGACode("NG-LA-IKE") {
		t.Fatal("NG-LA-IKE rejected")
	}
	// structural extension path: uncatalogued but well-formed on a known state
	if !ValidLGACode("NG-KE-ABC") {
		t.Fatal("structural LGA on known state rejected")
	}
	for _, bad := range []string{"", "NG-XX-ABC", "AB-ANO", "NG-AB-AN", "NG-AB-ANOO"} {
		if ValidLGACode(bad) {
			t.Fatalf("bad LGA %q accepted", bad)
		}
	}
}

func TestCurrencyHSNPaymentStatusCatalogs(t *testing.T) {
	if !ValidCurrency("NGN") || !ValidCurrency("USD") {
		t.Fatal("known currencies rejected")
	}
	if ValidCurrency("XXX") || ValidCurrency("NG") {
		t.Fatal("unknown currency accepted")
	}
	if !ValidHSNCode("2523") || !ValidHSNCode("998314") {
		t.Fatal("known HSN rejected")
	}
	if ValidHSNCode("12") || ValidHSNCode("ABCDEF") {
		t.Fatal("bad HSN accepted")
	}
	for _, s := range []string{"PENDING", "PAID", "REJECTED"} {
		if !ValidPaymentStatus(s) {
			t.Fatalf("payment status %s rejected", s)
		}
	}
	if ValidPaymentStatus("PARTIAL") {
		t.Fatal("unknown payment status accepted")
	}
}
