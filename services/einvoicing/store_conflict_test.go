package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestPgUniqueViolation(t *testing.T) {
	err := fmt.Errorf("wrap: %w", &pgconn.PgError{Code: "23505", ConstraintName: "einvoicing_irn_ux"})
	c, ok := pgUniqueViolation(err)
	if !ok || c != "einvoicing_irn_ux" {
		t.Fatalf("got (%q,%v)", c, ok)
	}
	if _, ok := pgUniqueViolation(errors.New("boom")); ok {
		t.Fatal("plain error must not be a unique violation")
	}
	if _, ok := pgUniqueViolation(&pgconn.PgError{Code: "40001"}); ok {
		t.Fatal("non-23505 pg error must not be a unique violation")
	}
}

func TestErrConflictIdentity(t *testing.T) {
	err := fmt.Errorf("%w (constraint einvoicing_suppnum_ux)", ErrConflict)
	if !errors.Is(err, ErrConflict) {
		t.Fatal("wrapped conflict must match errors.Is")
	}
	if errors.Is(err, ErrIdempotentReplay) {
		t.Fatal("conflict must not match replay")
	}
}

// TestIdempotencyPayloadBinding (audit w2 #5): same Idempotency-Key with a
// different payload must be a 409-class conflict, not a silent replay of the
// prior invoice; same key + same payload remains a normal replay.
func TestIdempotencyPayloadBinding(t *testing.T) {
	dir := t.TempDir()
	store, err := NewInvoiceStore(dir + "/invoices.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	inv := sampleInvoice()
	inv.TenantID = "t1"
	inv.IdempotencyKey = "key-abc"
	if _, err := store.Save(inv); err != nil {
		t.Fatal(err)
	}

	// same key, same payload -> replay returns prior id
	replay := sampleInvoice()
	replay.TenantID = "t1"
	replay.IdempotencyKey = "key-abc"
	replay.ID = "different-id"
	priorID, err := store.Save(replay)
	if !errors.Is(err, ErrIdempotentReplay) || priorID != inv.ID {
		t.Fatalf("same-payload replay: priorID=%q err=%v", priorID, err)
	}

	// same key, DIFFERENT payload -> payload conflict (409)
	changed := sampleInvoice()
	changed.TenantID = "t1"
	changed.IdempotencyKey = "key-abc"
	changed.ID = "another-id"
	changed.Lines[0].UnitPriceKobo += 1000
	changed.Normalise()
	if changed.CoreHash() == inv.CoreHash() {
		t.Fatal("test setup: changed payload must change CoreHash")
	}
	priorID, err = store.Save(changed)
	if !errors.Is(err, ErrIdempotencyPayloadConflict) {
		t.Fatalf("different-payload replay: expected payload conflict, got priorID=%q err=%v", priorID, err)
	}

	// binding survives a reload (hash derived from persisted fields)
	store2, err := NewInvoiceStore(dir + "/invoices.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store2.Save(changed)
	if !errors.Is(err, ErrIdempotencyPayloadConflict) {
		t.Fatalf("after reload: expected payload conflict, got %v", err)
	}
}
