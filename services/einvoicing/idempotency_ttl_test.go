package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// R4: an idempotency-key mapping older than idempotencyTTL is treated as
// new — a reused key starts a fresh invoice instead of replaying the prior
// one.
func TestIdempotencyKeyExpiredTreatedAsNew(t *testing.T) {
	dir := t.TempDir()
	store, err := NewInvoiceStore(filepath.Join(dir, "invoices.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	first := sampleInvoice()
	first.TenantID = "t1"
	first.IdempotencyKey = "key-ttl"
	if _, err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	// fresh key: replay within the window
	second := sampleInvoice()
	second.TenantID = "t1"
	second.IdempotencyKey = "key-ttl"
	if _, err := store.Save(second); !errors.Is(err, ErrIdempotentReplay) {
		t.Fatalf("fresh key must replay, got %v", err)
	}
	// age the backing invoice beyond the TTL
	aged := *first
	aged.CreatedAt = time.Now().Add(-2 * idempotencyTTL)
	store.byID[first.ID] = &aged
	third := sampleInvoice()
	third.TenantID = "t1"
	third.IdempotencyKey = "key-ttl"
	third.InvoiceNumber = "INV-2026-0002"
	third.ID = "inv-new"
	if _, err := store.Save(third); err != nil {
		t.Fatalf("expired key must be treated as new, got %v", err)
	}
	if id := store.byIdemKey["t1|key-ttl"]; id != "inv-new" {
		t.Fatalf("mapping must be re-pointed to the new invoice, got %q", id)
	}
}

// R4: the purge drops only expired mappings whose invoice is terminal;
// in-flight invoices keep their mapping regardless of age.
func TestPurgeExpiredIdempotencyKeysTerminalOnly(t *testing.T) {
	dir := t.TempDir()
	store, err := NewInvoiceStore(filepath.Join(dir, "invoices.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * idempotencyTTL)
	mk := func(id, key, status string, created time.Time) {
		inv := sampleInvoice()
		inv.ID, inv.TenantID, inv.IdempotencyKey, inv.Status = id, "t1", key, status
		inv.InvoiceNumber = id
		if _, err := store.Save(inv); err != nil {
			t.Fatal(err)
		}
		aged := *store.byID[id]
		aged.CreatedAt = created
		store.byID[id] = &aged
	}
	mk("i1", "k1", "reported", old)             // expired terminal -> purge
	mk("i2", "k2", "failed", old)               // expired terminal -> purge
	mk("i3", "k3", "validated", old)            // expired in-flight -> keep
	mk("i4", "k4", "reported", time.Now())      // fresh terminal -> keep

	if n := store.PurgeExpiredIdempotencyKeys(time.Now()); n != 2 {
		t.Fatalf("expected 2 purged, got %d", n)
	}
	for _, k := range []string{"t1|k1", "t1|k2"} {
		if _, ok := store.byIdemKey[k]; ok {
			t.Fatalf("expired terminal mapping %s must be purged", k)
		}
	}
	for _, k := range []string{"t1|k3", "t1|k4"} {
		if _, ok := store.byIdemKey[k]; !ok {
			t.Fatalf("mapping %s must be retained", k)
		}
	}
	// invoices themselves are never deleted
	if len(store.List()) != 4 {
		t.Fatalf("purge must not delete invoices, got %d", len(store.List()))
	}
}
