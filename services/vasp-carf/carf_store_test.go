package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression test for the assurance HIGH finding: CARFStore was purely
// in-memory, so built/transmitted CARF messages were lost on restart.
// Fails against the pre-fix code: NewCARFStore() took no dir and had no
// replay, so a "restarted" store comes back empty.
func TestCARFStoreDurableAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s1 := NewCARFStore(dir)
	rec := &CARFRecord{ID: "m1", MessageRefId: "ref-1", Period: "2025-Q1", TenantID: "t1",
		DocTypeIndic: "OECD1", Status: "built", BuiltAt: "2025-04-01T00:00:00Z", XML: "<x/>"}
	if err := s1.Add(rec); err != nil {
		t.Fatalf("add: %v", err)
	}
	// simulate process restart: new store over same dir
	s2 := NewCARFStore(dir)
	got, ok := s2.Get("m1")
	if !ok {
		t.Fatal("record lost after restart (store not durable)")
	}
	if got.MessageRefId != "ref-1" || got.Status != "built" {
		t.Fatalf("replayed record mismatch: %+v", got)
	}
	// last-write-wins on status transition
	rec.Status = "transmitted"
	if err := s2.Add(rec); err != nil {
		t.Fatalf("update: %v", err)
	}
	s3 := NewCARFStore(dir)
	got3, ok := s3.Get("m1")
	if !ok || got3.Status != "transmitted" {
		t.Fatalf("status transition not durable: ok=%v rec=%+v", ok, got3)
	}
	// log file actually exists on disk
	if _, err := os.Stat(filepath.Join(dir, "carf_records.log")); err != nil {
		t.Fatalf("append-log missing: %v", err)
	}
}
