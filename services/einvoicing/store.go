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

	"github.com/jackc/pgx/v5/pgconn"
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
	byIRN     map[string]string // IRN -> invoice id (NRS parity)
	order     []string
	docs      *prodx.DocStore // non-nil when DATABASE_URL set (prod)

	// faultHook, when non-nil (tests only), is consulted before each Save;
	// a non-nil return simulates a database fault (timeout / deadlock) and
	// the write is refused WITHOUT mutating state (assurance R7 §6.3
	// db-fault-injection cells).
	faultHook func(op string) error
}

// setFaultHook installs a test-only fault-injection hook (op == "save").
func (s *InvoiceStore) setFaultHook(h func(op string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faultHook = h
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
		byIRN: map[string]string{},
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
	if inv.IRN != "" {
		s.byIRN[inv.IRN] = inv.ID
	}
}

// GetByIRN resolves an invoice by its NRS IRN.
func (s *InvoiceStore) GetByIRN(irn string) (*CanonicalInvoice, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byIRN[irn]
	if !ok {
		return nil, false
	}
	cp := *s.byID[id]
	return &cp, true
}

// idempotencyTTL bounds how long an Idempotency-Key replay window stays
// open (assurance R4). After it lapses the key is treated as new; expired
// key mappings become purge-eligible once the invoice they point at is in
// a terminal status.
const idempotencyTTL = 7 * 24 * time.Hour

// terminalInvoiceStatuses are the lifecycle states after which an invoice
// no longer changes: the idempotency mapping may then be safely purged.
var terminalInvoiceStatuses = map[string]bool{"reported": true, "failed": true}

// idemMappingExpired reports whether the invoice backing an idempotency
// mapping is older than the replay window.
func idemMappingExpired(inv *CanonicalInvoice, now time.Time) bool {
	if inv.CreatedAt.IsZero() {
		return false
	}
	return now.Sub(inv.CreatedAt) > idempotencyTTL
}

// ErrIdempotentReplay is returned (with the prior invoice) when an
// Idempotency-Key was already consumed.
var ErrIdempotentReplay = errors.New("idempotency key already used")

// ErrIdempotencyPayloadConflict is returned when an Idempotency-Key is
// replayed with a DIFFERENT invoice payload (audit w2 #5: previously the
// prior invoice was returned regardless of payload). Payload binding uses
// CanonicalInvoice.CoreHash — the same locked-core-field hash that enforces
// post-signage immutability. HTTP layer maps this to 409.
var ErrIdempotencyPayloadConflict = errors.New("idempotency key already used with a different payload")

// ErrConflict is returned when Postgres rejects a write on the unique
// indexes (IRN / supplier+invoice number) enforced by migration
// 0001_einvoicing_uniqueness — i.e. a duplicate caught DB-side in
// multi-instance deployments where the in-memory maps are not a constraint.
var ErrConflict = errors.New("invoice conflicts with an existing record")

// pgUniqueViolation extracts the constraint name from a Postgres 23505
// unique-violation error.
func pgUniqueViolation(err error) (constraint string, ok bool) {
	var pge *pgconn.PgError
	if errors.As(err, &pge) && pge.Code == "23505" {
		return pge.ConstraintName, true
	}
	return "", false
}

// Save persists (append+fsync) and indexes the invoice. Returns the prior
// invoice id if the idempotency key already exists.
func (s *InvoiceStore) Save(inv *CanonicalInvoice) (priorID string, err error) {
	return s.SaveCtx(context.Background(), inv)
}

// SaveCtx is Save with caller context threading (QA-27): the DB persist path
// honours request cancellation/deadlines.
func (s *InvoiceStore) SaveCtx(ctx context.Context, inv *CanonicalInvoice) (priorID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.faultHook != nil {
		if ferr := s.faultHook("save"); ferr != nil {
			return "", ferr
		}
	}
	if inv.IdempotencyKey != "" {
		if id, dup := s.byIdemKey[inv.TenantID+"|"+inv.IdempotencyKey]; dup && id != inv.ID {
			prior, ok := s.byID[id]
			// R4 TTL: an expired mapping no longer dedups — the reused key
			// starts a fresh invoice and the mapping is re-pointed below.
			if ok && !idemMappingExpired(prior, time.Now()) {
				// Payload binding: same key + different core payload -> 409-class
				// conflict, never a silent replay of the prior invoice.
				if prior.CoreHash() != inv.CoreHash() {
					return id, ErrIdempotencyPayloadConflict
				}
				return id, ErrIdempotentReplay
			}
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
		if err := s.docs.Put(ctx, "invoices", inv.ID, line); err != nil {
			if constraint, ok := pgUniqueViolation(err); ok {
				// DB-side uniqueness (multi-instance safe): map to the
				// same conflict semantics the in-memory maps provide.
				if constraint == "einvoicing_idem_ux" {
					priorID := s.byIdemKey[inv.TenantID+"|"+inv.IdempotencyKey]
					if prior, ok := s.byID[priorID]; ok && prior.CoreHash() != inv.CoreHash() {
						return priorID, ErrIdempotencyPayloadConflict
					}
					return priorID, ErrIdempotentReplay
				}
				return "", fmt.Errorf("%w (constraint %s)", ErrConflict, constraint)
			}
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

// PurgeExpiredIdempotencyKeys drops idempotency-key mappings whose replay
// window has closed AND whose invoice is in a terminal status. In-flight
// invoices (received/validated/precleared) keep their mapping so a late
// retry still resolves to the original invoice. The invoice records
// themselves are never touched. Returns the number of mappings purged.
func (s *InvoiceStore) PurgeExpiredIdempotencyKeys(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	purged := 0
	for key, id := range s.byIdemKey {
		inv, ok := s.byID[id]
		if !ok || !terminalInvoiceStatuses[inv.Status] || !idemMappingExpired(inv, now) {
			continue
		}
		delete(s.byIdemKey, key)
		purged++
	}
	return purged
}

// IsDuplicate reports whether supplier TIN + invoice number already exists.
func (s *InvoiceStore) IsDuplicate(inv *CanonicalInvoice) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.bySuppNum[inv.TenantID+"|"+inv.Supplier.TIN+"|"+inv.InvoiceNumber]
	return ok && id != inv.ID
}
