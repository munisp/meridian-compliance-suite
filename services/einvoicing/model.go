package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/munisp/meridian-compliance-suite/packages/shared/envelope"
)

// Party is a buyer or supplier on the canonical invoice (SPEC §3 T1).
type Party struct {
	TIN     string `json:"tin,omitempty"`
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
	City    string `json:"city,omitempty"`
	State   string `json:"state,omitempty"`
	Country string `json:"country,omitempty"` // ISO 3166-1 alpha-2, default NG
	Email   string `json:"email,omitempty"`
}

// InvoiceLine: money is integer kobo ONLY (SPEC §1.3). Quantity in milli-units
// (1000 = 1 unit) so fractional quantities stay integral.
type InvoiceLine struct {
	ID            string `json:"id"`
	Description   string `json:"description"`
	QuantityMilli int64  `json:"quantity_milli"`
	UnitPriceKobo int64  `json:"unit_price_kobo"`
	LineTotalKobo int64  `json:"line_total_kobo"` // = QuantityMilli*UnitPriceKobo/1000
	VatCategory   string `json:"vat_category"`    // S=standard 7.5%, Z=zero, E=exempt
	VatRateBps    int64  `json:"vat_rate_bps"`    // 750 = 7.5%
	VatAmountKobo int64  `json:"vat_amount_kobo"`
}

// CryptoStamp is the MBS cryptographic stamp returned with the IRN.
type CryptoStamp struct {
	Algorithm string `json:"algorithm"` // ed25519
	KeyID     string `json:"key_id"`
	IRN       string `json:"irn"`
	Payload   string `json:"payload"`   // canonical string that was signed
	Signature string `json:"signature"` // hex
	StampedAt string `json:"stamped_at"`
}

// CanonicalInvoice is the ERP-independent invoice model.
type CanonicalInvoice struct {
	ID               string        `json:"id"`
	TenantID         string        `json:"tenant_id"`
	InvoiceNumber    string        `json:"invoice_number"`
	InvoiceType      string        `json:"invoice_type"` // B2B|B2C|B2G
	IssueDate        string        `json:"issue_date"`   // YYYY-MM-DD
	DueDate          string        `json:"due_date,omitempty"`
	Currency         string        `json:"currency"`
	Supplier         Party         `json:"supplier"`
	Customer         Party         `json:"customer"`
	Lines            []InvoiceLine `json:"lines"`
	TaxExclusiveKobo int64         `json:"tax_exclusive_kobo"`
	TaxKobo          int64         `json:"tax_kobo"`
	PayableKobo      int64         `json:"payable_kobo"`
	Status           string        `json:"status"` // received|validated|precleared|reported|failed
	IRN              string        `json:"irn,omitempty"`
	Stamp            *CryptoStamp  `json:"crypto_stamp,omitempty"`
	CSIDSignature    string        `json:"csid_signature,omitempty"` // supplier-side ed25519 sig
	CSIDKeyID        string        `json:"csid_key_id,omitempty"`
	IdempotencyKey   string        `json:"idempotency_key,omitempty"`
	SourceAdapter    string        `json:"source_adapter,omitempty"`
	// NRS parity fields (all optional; existing flows unaffected).
	BusinessID       string       `json:"business_id,omitempty"`
	ServiceID        string       `json:"service_id,omitempty"`        // NRS 8-char integrator id used in the IRN
	InvoiceTypeCode  string       `json:"invoice_type_code,omitempty"` // UNTDID 1001, e.g. 380|381|383
	PaymentStatus    string       `json:"payment_status,omitempty"`    // PENDING|PAID|REJECTED
	PaymentReference string       `json:"payment_reference,omitempty"`
	SignedCoreHash   string       `json:"signed_core_hash,omitempty"` // core-field hash at invoice signage (immutability)
	NRSPayload       string       `json:"nrs_payload,omitempty"`      // original NRS-schema JSON (resubmission/revalidation)
	Audit            []AuditEntry `json:"audit,omitempty"`
	UBLXML           string       `json:"ubl_xml,omitempty"`
	Validation       []Violation  `json:"validation,omitempty"`
	CreatedAt        time.Time    `json:"created_at"`
	UpdatedAt        time.Time    `json:"updated_at"`
}

// AuditEntry is one immutable audit-trail record (payment-status updates,
// lifecycle transitions).
type AuditEntry struct {
	At     string `json:"at"`
	Action string `json:"action"` // e.g. payment_status_update
	Detail string `json:"detail"`
	Actor  string `json:"actor,omitempty"`
}

// CoreHash hashes only the locked core fields (parties, lines, totals,
// number, dates). Computed at invoice-signage time; any later change to a
// core field changes the hash, which is how post-signage immutability is
// enforced. Mutable fields (payment_status, payment_reference, status) are
// excluded by construction.
func (inv *CanonicalInvoice) CoreHash() string {
	c := struct {
		Number   string        `json:"n"`
		Issued   string        `json:"i"`
		Due      string        `json:"d"`
		TypeCode string        `json:"tc"`
		Currency string        `json:"cur"`
		Supplier Party         `json:"s"`
		Customer Party         `json:"c"`
		Lines    []InvoiceLine `json:"l"`
		Excl     int64         `json:"e"`
		Tax      int64         `json:"t"`
		Payable  int64         `json:"p"`
	}{inv.InvoiceNumber, inv.IssueDate, inv.DueDate, inv.InvoiceTypeCode, inv.Currency,
		inv.Supplier, inv.Customer, inv.Lines, inv.TaxExclusiveKobo, inv.TaxKobo, inv.PayableKobo}
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Violation is one validation finding from rp-ubl-bis / rp-mbs-business-rules.
type Violation struct {
	Pack     string `json:"pack"`
	Message  string `json:"message"`
	Severity string `json:"severity"` // fatal|warning
}

// Normalise fills defaults and recomputes line/document totals (kobo, integer).
func (inv *CanonicalInvoice) Normalise() {
	if inv.ID == "" {
		inv.ID = envelope.ULID()
	}
	if inv.Currency == "" {
		inv.Currency = "NGN"
	}
	if inv.InvoiceType == "" {
		inv.InvoiceType = "B2B"
	}
	if inv.IssueDate == "" {
		inv.IssueDate = time.Now().UTC().Format("2006-01-02")
	}
	if inv.Supplier.Country == "" {
		inv.Supplier.Country = "NG"
	}
	if inv.Customer.Country == "" {
		inv.Customer.Country = "NG"
	}
	var excl, tax int64
	for i := range inv.Lines {
		l := &inv.Lines[i]
		if l.ID == "" {
			l.ID = fmt.Sprintf("%d", i+1)
		}
		if l.VatCategory == "" {
			l.VatCategory = "S"
		}
		if l.VatCategory == "S" && l.VatRateBps == 0 {
			l.VatRateBps = 750 // 7.5% standard
		}
		if l.LineTotalKobo == 0 && l.QuantityMilli != 0 {
			l.LineTotalKobo = l.QuantityMilli * l.UnitPriceKobo / 1000
		}
		l.VatAmountKobo = RoundBpsHalfUp(l.LineTotalKobo, l.VatRateBps)
		excl += l.LineTotalKobo
		tax += l.VatAmountKobo
	}
	if inv.TaxExclusiveKobo == 0 {
		inv.TaxExclusiveKobo = excl
	}
	if inv.TaxKobo == 0 {
		inv.TaxKobo = tax
	}
	if inv.PayableKobo == 0 {
		inv.PayableKobo = inv.TaxExclusiveKobo + inv.TaxKobo
	}
}

// RoundBpsHalfUp computes amountKobo*rateBps/10000 with round-half-up, mirroring
// services/pos-vat.roundBpsHalfUp and the pack-mandated round() in
// rp-mbs-business-rules (mbs.vat.arithmetic).
func RoundBpsHalfUp(amountKobo, rateBps int64) int64 {
	if amountKobo >= 0 {
		return (amountKobo*rateBps + 5000) / 10000
	}
	return -((-amountKobo*rateBps + 5000) / 10000)
}

// TotalsConsistent checks declared totals against recomputed line totals.
func (inv *CanonicalInvoice) TotalsConsistent() bool {
	var excl int64
	for _, l := range inv.Lines {
		excl += l.LineTotalKobo
	}
	return excl == inv.TaxExclusiveKobo && inv.PayableKobo == inv.TaxExclusiveKobo+inv.TaxKobo
}

// Hash is the canonical content hash (deterministic key order).
func (inv *CanonicalInvoice) Hash() string {
	c := struct {
		Number   string `json:"n"`
		Date     string `json:"d"`
		Supplier string `json:"s"`
		Customer string `json:"c"`
		Payable  int64  `json:"p"`
		Currency string `json:"cur"`
	}{inv.InvoiceNumber, inv.IssueDate, inv.Supplier.TIN, inv.Customer.TIN, inv.PayableKobo, inv.Currency}
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// RuleContext flattens the invoice for rp-* evaluation.
func (inv *CanonicalInvoice) RuleContext(duplicate bool) map[string]any {
	ctx := map[string]any{
		"invoice_number": inv.InvoiceNumber, "issue_date": inv.IssueDate,
		"currency": inv.Currency, "invoice_type": inv.InvoiceType,
		"supplier_tin": inv.Supplier.TIN, "supplier_name": inv.Supplier.Name,
		"customer_tin": inv.Customer.TIN, "customer_name": inv.Customer.Name,
		"payable_amount_kobo": inv.PayableKobo, "line_count": len(inv.Lines),
		"totals_consistent": inv.TotalsConsistent(), "duplicate": duplicate,
	}
	if len(inv.Supplier.TIN) > 0 {
		ctx["supplier_tin_len"] = len(strings.TrimSpace(inv.Supplier.TIN))
	}
	var maxRate int64
	cats := map[string]bool{}
	for _, l := range inv.Lines {
		if l.VatRateBps > maxRate {
			maxRate = l.VatRateBps
		}
		cats[l.VatCategory] = true
	}
	ctx["vat_rate_bps"] = maxRate
	if len(cats) == 1 {
		for c := range cats {
			ctx["vat_category"] = c
		}
	} else {
		keys := make([]string, 0, len(cats))
		for c := range cats {
			keys = append(keys, c)
		}
		sort.Strings(keys)
		ctx["vat_category"] = keys[0]
	}
	return ctx
}
