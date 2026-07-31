package main

// NRS Invoice Reference Number (IRN) — official NRS/Gention format:
//
//	IRN = <InvoiceNumber>-<ServiceID>-<DateStamp>
//
// where ServiceID is the 8-character alphanumeric identifier NRS assigns to
// the system integrator/business (e.g. 94ND90NR) and DateStamp is the invoice
// issue date as YYYYMMDD. Example: INV0001-94ND90NR-20260127.

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

var serviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{8}$`)

// ValidServiceID reports whether s is a well-formed NRS service id
// (exactly 8 alphanumeric characters).
func ValidServiceID(s string) bool { return serviceIDPattern.MatchString(s) }

// BuildIRN composes the IRN from its three parts. invoiceNumber is used
// verbatim (it must not itself contain a hyphen-run that would make the IRN
// ambiguous — callers should keep invoice numbers hyphen-free per NRS
// guidance; ParseIRN tolerates hyphens by splitting on the LAST two).
func BuildIRN(invoiceNumber, serviceID, issueDate string) (string, error) {
	invoiceNumber = strings.TrimSpace(invoiceNumber)
	if invoiceNumber == "" {
		return "", fmt.Errorf("invoice number is required for IRN")
	}
	if !ValidServiceID(serviceID) {
		return "", fmt.Errorf("service id %q invalid: must be 8 alphanumeric characters", serviceID)
	}
	ds, err := DateStamp(issueDate)
	if err != nil {
		return "", err
	}
	return invoiceNumber + "-" + serviceID + "-" + ds, nil
}

// DateStamp converts an issue date (YYYY-MM-DD or YYYYMMDD) to YYYYMMDD,
// validating that it is a real calendar date.
func DateStamp(issueDate string) (string, error) {
	d := strings.TrimSpace(strings.ReplaceAll(issueDate, "-", ""))
	if len(d) != 8 {
		return "", fmt.Errorf("date stamp %q invalid: want YYYYMMDD", issueDate)
	}
	if _, err := time.Parse("20060102", d); err != nil {
		return "", fmt.Errorf("date stamp %q invalid: %v", issueDate, err)
	}
	return d, nil
}

// ParseIRN splits an IRN into invoice number, service id and date stamp,
// validating the service-id and date-stamp structure. The invoice number may
// contain hyphens; the last two hyphen-separated segments are the service id
// and date stamp.
func ParseIRN(irn string) (invoiceNumber, serviceID, dateStamp string, err error) {
	irn = strings.TrimSpace(irn)
	i := strings.LastIndex(irn, "-")
	if i <= 0 || i == len(irn)-1 {
		return "", "", "", fmt.Errorf("irn %q malformed: want <InvoiceNumber>-<ServiceID>-<YYYYMMDD>", irn)
	}
	dateStamp = irn[i+1:]
	rest := irn[:i]
	j := strings.LastIndex(rest, "-")
	if j <= 0 || j == len(rest)-1 {
		return "", "", "", fmt.Errorf("irn %q malformed: want <InvoiceNumber>-<ServiceID>-<YYYYMMDD>", irn)
	}
	serviceID = rest[j+1:]
	invoiceNumber = rest[:j]
	if !ValidServiceID(serviceID) {
		return "", "", "", fmt.Errorf("irn %q malformed: service id %q must be 8 alphanumeric characters", irn, serviceID)
	}
	if _, err := DateStamp(dateStamp); err != nil {
		return "", "", "", fmt.Errorf("irn %q malformed: %v", irn, err)
	}
	return invoiceNumber, serviceID, dateStamp, nil
}

// ValidIRN reports structural validity only (not uniqueness).
func ValidIRN(irn string) bool {
	_, _, _, err := ParseIRN(irn)
	return err == nil
}

// ServiceIDRegistry maps business_id → NRS-assigned 8-char service id. In
// production the id is issued by NRS at integrator onboarding and injected
// via config (NRS_SERVICE_ID / per-business registration); the dev registry
// auto-assigns a random id on first use so the full pipeline is exercisable.
type ServiceIDRegistry struct {
	mu      sync.RWMutex
	byBizID map[string]string
}

func NewServiceIDRegistry() *ServiceIDRegistry {
	return &ServiceIDRegistry{byBizID: map[string]string{}}
}

// Register binds an NRS-issued service id to a business. The id is validated.
func (r *ServiceIDRegistry) Register(businessID, serviceID string) error {
	if strings.TrimSpace(businessID) == "" {
		return fmt.Errorf("business_id required")
	}
	if !ValidServiceID(serviceID) {
		return fmt.Errorf("service id %q invalid: must be 8 alphanumeric characters", serviceID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byBizID[businessID]; ok && existing != serviceID {
		return fmt.Errorf("business %s already registered with service id %s", businessID, existing)
	}
	r.byBizID[businessID] = serviceID
	return nil
}

// Lookup returns the registered service id ("" when unknown).
func (r *ServiceIDRegistry) Lookup(businessID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byBizID[businessID]
}

// GetOrAssign returns the business's service id, assigning a random one in
// dev when none is registered yet.
func (r *ServiceIDRegistry) GetOrAssign(businessID string) (string, error) {
	if id := r.Lookup(businessID); id != "" {
		return id, nil
	}
	id, err := randomServiceID()
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byBizID[businessID]; ok {
		return existing, nil
	}
	r.byBizID[businessID] = id
	return id, nil
}

const serviceIDAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomServiceID() (string, error) {
	buf := make([]byte, 8)
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	for i, b := range raw {
		buf[i] = serviceIDAlphabet[int(b)%len(serviceIDAlphabet)]
	}
	return string(buf), nil
}
