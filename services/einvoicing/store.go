package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/munisp/meridian-compliance-suite/packages/prodx"
)

// InvoiceStore is the durable canonical store (dev: JSONL file + in-memory
// index; the Postgres outbox pattern of SPEC §1.1 is represented by the
// file outbox in replay.go). Append-only log, last write wins by id.
type InvoiceStore struct {
	path      string
	mu        sync.RWMutex
	byID      map[string]*CanonicalInvoice
	byIdemKey map[string]string // idempotency key -> invoice id
	bySuppNum map[string]string // supplierTIN|invoiceNumber -> id
	order     []string
	docs      *prodx.DocStore // non-nil when DATABASE_URL set (prod)
}

// SetPG attaches the Postgres document store (H1: DATABASE_URL). When set,
// Postgres is the durable write path and the JSONL file becomes a local
// debug mirror; startup loads from Postgres.
func (s *InvoiceStore) SetPG(ctx context.Context, docs *prodx.DocStore) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs = docs
	docsList, err := docs.List(ctx, "invoices")
	if err != nil {
		return err
	}
	for _, raw := range docsList {
		var inv CanonicalInvoice
		if err := json.Unmarshal(raw, &inv); err != nil {
			return fmt.Errorf("pg store corrupt: %w", err)
		}
		s.index(&inv)
	}
	return nil
}

func NewInvoiceStore(path string) (*InvoiceStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &InvoiceStore{
		path: path, byID: map[string]*CanonicalInvoice{},
		byIdemKey: map[string]string{}, bySuppNum: map[string]string{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *InvoiceStore) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var inv CanonicalInvoice
		if err := json.Unmarshal([]byte(line), &inv); err != nil {
			return fmt.Errorf("store corrupt: %w", err)
		}
		s.index(&inv)
	}
	return nil
}

func (s *InvoiceStore) index(inv *CanonicalInvoice) {
	if _, exists := s.byID[inv.ID]; !exists {
		s.order = append(s.order, inv.ID)
	}
	cp := *inv
	s.byID[inv.ID] = &cp
	if inv.IdempotencyKey != "" {
		// tenant-prefixed: idempotency keys are scoped per tenant (audit fix)
		s.byIdemKey[inv.TenantID+"|"+inv.IdempotencyKey] = inv.ID
	}
	if inv.Supplier.TIN != "" && inv.InvoiceNumber != "" {
		// tenant-prefixed: same supplier TIN + number in another tenant is NOT a duplicate
		s.bySuppNum[inv.TenantID+"|"+inv.Supplier.TIN+"|"+inv.InvoiceNumber] = inv.ID
	}
}

// ErrIdempotentReplay is returned (with the prior invoice) when an
// Idempotency-Key was already consumed.
var ErrIdempotentReplay = errors.New("idempotency key already used")

// Save persists (append+fsync) and indexes the invoice. Returns the prior
// invoice id if the idempotency key already exists.
func (s *InvoiceStore) Save(inv *CanonicalInvoice) (priorID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inv.IdempotencyKey != "" {
		if id, dup := s.byIdemKey[inv.TenantID+"|"+inv.IdempotencyKey]; dup && id != inv.ID {
			return id, ErrIdempotentReplay
		}
	}
	inv.UpdatedAt = time.Now().UTC()
	if inv.CreatedAt.IsZero() {
		inv.CreatedAt = inv.UpdatedAt
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	line, err := json.Marshal(inv)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if s.docs != nil {
		if err := s.docs.Put(context.Background(), "invoices", inv.ID, line); err != nil {
			return "", fmt.Errorf("pg persist: %w", err)
		}
	}
	s.index(inv)
	return "", nil
}

func (s *InvoiceStore) Get(id string) (*CanonicalInvoice, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	inv, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	cp := *inv
	return &cp, true
}

func (s *InvoiceStore) List() []*CanonicalInvoice {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*CanonicalInvoice, 0, len(s.order))
	for _, id := range s.order {
		cp := *s.byID[id]
		out = append(out, &cp)
	}
	return out
}

// IsDuplicate reports whether supplier TIN + invoice number already exists.
func (s *InvoiceStore) IsDuplicate(inv *CanonicalInvoice) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.bySuppNum[inv.TenantID+"|"+inv.Supplier.TIN+"|"+inv.InvoiceNumber]
	return ok && id != inv.ID
}
