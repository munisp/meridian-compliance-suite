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
