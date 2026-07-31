package main

import (
	"strings"
	"testing"
)

func TestBuildIRNValid(t *testing.T) {
	irn, err := BuildIRN("INV0001", "94ND90NR", "2026-01-27")
	if err != nil {
		t.Fatal(err)
	}
	if irn != "INV0001-94ND90NR-20260127" {
		t.Fatalf("irn=%s", irn)
	}
}

func TestBuildIRNRejectsBadServiceID(t *testing.T) {
	for _, sid := range []string{"", "SHORT", "TOOLONG123", "94ND90N-", "94ND 0NR"} {
		if _, err := BuildIRN("INV0001", sid, "2026-01-27"); err == nil {
			t.Fatalf("expected error for service id %q", sid)
		}
	}
}

func TestBuildIRNRejectsBadDate(t *testing.T) {
	for _, d := range []string{"", "2026-13-01", "2026-02-30", "27/01/2026"} {
		if _, err := BuildIRN("INV0001", "94ND90NR", d); err == nil {
			t.Fatalf("expected error for date %q", d)
		}
	}
}

func TestParseIRNRoundTrip(t *testing.T) {
	num, sid, ds, err := ParseIRN("INV0001-94ND90NR-20260127")
	if err != nil {
		t.Fatal(err)
	}
	if num != "INV0001" || sid != "94ND90NR" || ds != "20260127" {
		t.Fatalf("got %q %q %q", num, sid, ds)
	}
	if !ValidIRN("INV0001-94ND90NR-20260127") {
		t.Fatal("ValidIRN false for well-formed IRN")
	}
}

func TestParseIRNHyphenatedInvoiceNumber(t *testing.T) {
	num, sid, ds, err := ParseIRN("INV-2026-0001-94ND90NR-20260127")
	if err != nil {
		t.Fatal(err)
	}
	if num != "INV-2026-0001" || sid != "94ND90NR" || ds != "20260127" {
		t.Fatalf("got %q %q %q", num, sid, ds)
	}
}

func TestParseIRNMalformed(t *testing.T) {
	for _, irn := range []string{
		"", "INV0001", "INV0001-20260127", "-94ND90NR-20260127",
		"INV0001-94ND90NR-20261340", // month 13, day 40
		"INV0001-94ND90NR-20260230", // Feb 30
		"INV0001-SHORT-20260127",
		"INV0001-94ND90NR-", "INV0001--20260127",
	} {
		if _, _, _, err := ParseIRN(irn); err == nil {
			t.Fatalf("expected parse error for %q", irn)
		}
		if ValidIRN(irn) {
			t.Fatalf("ValidIRN true for %q", irn)
		}
	}
}

func TestDateStamp(t *testing.T) {
	if ds, err := DateStamp("2026-01-27"); err != nil || ds != "20260127" {
		t.Fatalf("ds=%q err=%v", ds, err)
	}
	if ds, err := DateStamp("20260127"); err != nil || ds != "20260127" {
		t.Fatalf("ds=%q err=%v", ds, err)
	}
}

func TestServiceIDRegistryRegisterAndConflict(t *testing.T) {
	r := NewServiceIDRegistry()
	if err := r.Register("biz-1", "94ND90NR"); err != nil {
		t.Fatal(err)
	}
	if got := r.Lookup("biz-1"); got != "94ND90NR" {
		t.Fatalf("lookup=%q", got)
	}
	if err := r.Register("biz-1", "ABCD1234"); err == nil {
		t.Fatal("expected conflict on re-register with different id")
	}
	if err := r.Register("biz-2", "bad"); err == nil {
		t.Fatal("expected validation error for bad service id")
	}
}

func TestServiceIDRegistryAutoAssign(t *testing.T) {
	r := NewServiceIDRegistry()
	id1, err := r.GetOrAssign("biz-x")
	if err != nil {
		t.Fatal(err)
	}
	if !ValidServiceID(id1) {
		t.Fatalf("auto id %q invalid", id1)
	}
	id2, _ := r.GetOrAssign("biz-x")
	if id1 != id2 {
		t.Fatalf("auto id not stable: %q vs %q", id1, id2)
	}
	id3, _ := r.GetOrAssign("biz-y")
	if id3 == id1 {
		t.Fatal("different businesses got same auto id (astronomically unlikely; check rng)")
	}
}

func TestBuildIRNExampleFromSpec(t *testing.T) {
	// Spec example: INV0001-94ND90NR-20260127
	irn, err := BuildIRN("INV0001", "94ND90NR", "2026-01-27")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(irn, "-")
	if len(parts) != 3 || parts[1] != "94ND90NR" || parts[2] != "20260127" {
		t.Fatalf("bad shape: %s", irn)
	}
}
