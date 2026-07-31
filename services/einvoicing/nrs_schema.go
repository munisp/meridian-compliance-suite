package main

// NRS payload schema (UBL-shaped JSON, decimal NGN floats) ⇄ canonical
// kobo-integer model. Conversion happens ONLY at this boundary, with
// round-half-up; floats are never stored internally (SPEC §1.3).
//
// Field-order independence comes free from encoding/json.

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// NGNToKobo converts a decimal NGN amount to integer kobo, round-half-up.
// The conversion is done in decimal (not binary) space so that values like
// 0.575 — unrepresentable in float64 — still round half-up to 58 kobo, and
// float noise like 245236.28024999998 collapses to the intended 24523628.
func NGNToKobo(ngn float64) int64 {
	s := strconv.FormatFloat(ngn, 'f', 6, 64) // 6dp is far sub-kobo precision
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, fracPart, _ := strings.Cut(s, ".")
	units, _ := strconv.ParseInt(intPart, 10, 64)
	kobo := units * 100
	frac := (fracPart + "000000")[:6]
	cents, _ := strconv.ParseInt(frac[:2], 10, 64)
	if frac[2] >= '5' { // third decimal digit: round half-up
		cents++
	}
	kobo += cents
	if neg {
		return -kobo
	}
	return kobo
}

// KoboToNGN converts integer kobo back to decimal NGN for responses.
func KoboToNGN(kobo int64) float64 {
	return float64(kobo) / 100
}

// ---------------------------------------------------------------------------
// NRS schema structs (§3 of the NRS/Gention API spec)
// ---------------------------------------------------------------------------

type NRSPostalAddress struct {
	StreetName string `json:"street_name,omitempty"`
	CityName   string `json:"city_name,omitempty"`
	PostalZone string `json:"postal_zone,omitempty"`
	Country    string `json:"country,omitempty"`
	State      string `json:"state,omitempty"` // e.g. NG-AB
	LGA        string `json:"lga,omitempty"`   // e.g. NG-AB-ANO
}

type NRSParty struct {
	PartyName           string           `json:"party_name"`
	TIN                 string           `json:"tin"`
	Email               string           `json:"email,omitempty"`
	Telephone           string           `json:"telephone,omitempty"`
	BusinessDescription string           `json:"business_description,omitempty"`
	PostalAddress       NRSPostalAddress `json:"postal_address,omitempty"`
}

type NRSItem struct {
	Name                      string `json:"name"`
	Description               string `json:"description,omitempty"`
	SellersItemIdentification string `json:"sellers_item_identification,omitempty"`
}

type NRSPrice struct {
	PriceAmount  float64 `json:"price_amount"`
	BaseQuantity float64 `json:"base_quantity,omitempty"`
	PriceUnit    string  `json:"price_unit,omitempty"`
}

type NRSInvoiceLine struct {
	HSNCode             string   `json:"hsn_code,omitempty"`
	ProductCategory     string   `json:"product_category,omitempty"`
	DiscountRate        float64  `json:"discount_rate,omitempty"`
	DiscountAmount      float64  `json:"discount_amount,omitempty"`
	FeeRate             float64  `json:"fee_rate,omitempty"`
	FeeAmount           float64  `json:"fee_amount,omitempty"`
	InvoicedQuantity    float64  `json:"invoiced_quantity"`
	LineExtensionAmount float64  `json:"line_extension_amount"`
	Item                NRSItem  `json:"item"`
	Price               NRSPrice `json:"price"`
}

type NRSAllowanceCharge struct {
	Amount          float64 `json:"amount"`
	ChargeIndicator bool    `json:"charge_indicator"`
}

type NRSTaxCategory struct {
	ID      string  `json:"id"` // e.g. STANDARD_VAT
	Percent float64 `json:"percent"`
}

type NRSTaxSubtotal struct {
	TaxableAmount float64        `json:"taxable_amount"`
	TaxAmount     float64        `json:"tax_amount"`
	TaxCategory   NRSTaxCategory `json:"tax_category"`
}

type NRSTaxTotal struct {
	TaxAmount   float64          `json:"tax_amount"`
	TaxSubtotal []NRSTaxSubtotal `json:"tax_subtotal,omitempty"`
}

type NRSPaymentMeans struct {
	PaymentDueDate   string `json:"payment_due_date,omitempty"`
	PaymentMeansCode string `json:"payment_means_code"`
}

type NRSLegalMonetaryTotal struct {
	LineExtensionAmount float64 `json:"line_extension_amount"`
	TaxExclusiveAmount  float64 `json:"tax_exclusive_amount"`
	TaxInclusiveAmount  float64 `json:"tax_inclusive_amount"`
	PayableAmount       float64 `json:"payable_amount"`
}

type NRSInvoiceDeliveryPeriod struct {
	StartDate string `json:"start_date,omitempty"`
	EndDate   string `json:"end_date,omitempty"`
}

// NRSInvoice is the NRS/Gention UBL-shaped JSON payload.
type NRSInvoice struct {
	BusinessID              string                   `json:"business_id"`
	IRN                     string                   `json:"irn,omitempty"`
	IssueDate               string                   `json:"issue_date"`
	DueDate                 string                   `json:"due_date,omitempty"`
	IssueTime               string                   `json:"issue_time,omitempty"`
	InvoiceTypeCode         string                   `json:"invoice_type_code"` // e.g. "381"
	TaxPointDate            string                   `json:"tax_point_date,omitempty"`
	DocumentCurrencyCode    string                   `json:"document_currency_code"`
	TaxCurrencyCode         string                   `json:"tax_currency_code,omitempty"`
	InvoiceKind             string                   `json:"invoice_kind,omitempty"` // B2B
	PaymentStatus           string                   `json:"payment_status,omitempty"`
	Note                    string                   `json:"note,omitempty"`
	BuyerReference          string                   `json:"buyer_reference,omitempty"`
	OrderReference          string                   `json:"order_reference,omitempty"`
	AccountingCost          string                   `json:"accounting_cost,omitempty"`
	PaymentTermsNote        string                   `json:"payment_terms_note,omitempty"`
	ActualDeliveryDate      string                   `json:"actual_delivery_date,omitempty"`
	InvoiceDeliveryPeriod   NRSInvoiceDeliveryPeriod `json:"invoice_delivery_period,omitempty"`
	AccountingSupplierParty NRSParty                 `json:"accounting_supplier_party"`
	AccountingCustomerParty NRSParty                 `json:"accounting_customer_party"`
	InvoiceLine             []NRSInvoiceLine         `json:"invoice_line"`
	AllowanceCharge         []NRSAllowanceCharge     `json:"allowance_charge,omitempty"`
	TaxTotal                []NRSTaxTotal            `json:"tax_total,omitempty"`
	PaymentMeans            []NRSPaymentMeans        `json:"payment_means,omitempty"`
	LegalMonetaryTotal      NRSLegalMonetaryTotal    `json:"legal_monetary_total"`
}

// NRSError is one NRS-style validation finding.
type NRSError struct {
	Field   string `json:"field"`
	Code    string `json:"code"` // REQUIRED|INVALID|CATALOG
	Message string `json:"message"`
}

func (e NRSError) Error() string {
	return fmt.Sprintf("%s: %s (%s)", e.Field, e.Message, e.Code)
}

// NRSErrors aggregates every violation found (NRS-style error list).
type NRSErrors []NRSError

func (es NRSErrors) Error() string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return fmt.Sprintf("%d validation error(s): %s", len(es), strings.Join(parts, "; "))
}

func (es *NRSErrors) add(field, code, msg string) {
	*es = append(*es, NRSError{Field: field, Code: code, Message: msg})
}

// Validate checks the NRS payload against required fields and the reference
// catalogs, collecting EVERY violation (fail-fast happens at the caller after
// the full list is built — NRS returns the whole list in one response).
func (n *NRSInvoice) Validate() NRSErrors {
	var errs NRSErrors
	req := func(field, v string) {
		if strings.TrimSpace(v) == "" {
			errs.add(field, "REQUIRED", field+" is required")
		}
	}
	req("business_id", n.BusinessID)
	req("issue_date", n.IssueDate)
	req("invoice_type_code", n.InvoiceTypeCode)
	req("document_currency_code", n.DocumentCurrencyCode)
	if n.IRN != "" && !ValidIRN(n.IRN) {
		errs.add("irn", "INVALID", "irn must be <InvoiceNumber>-<ServiceID>-<YYYYMMDD>")
	}
	if n.InvoiceTypeCode != "" && !ValidInvoiceTypeCode(n.InvoiceTypeCode) {
		errs.add("invoice_type_code", "CATALOG", catalogError("invoice_type_code", n.InvoiceTypeCode, "invoice-type"))
	}
	if n.DocumentCurrencyCode != "" && !ValidCurrency(n.DocumentCurrencyCode) {
		errs.add("document_currency_code", "CATALOG", catalogError("document_currency_code", n.DocumentCurrencyCode, "currency"))
	}
	if n.TaxCurrencyCode != "" && !ValidCurrency(n.TaxCurrencyCode) {
		errs.add("tax_currency_code", "CATALOG", catalogError("tax_currency_code", n.TaxCurrencyCode, "currency"))
	}
	if n.PaymentStatus != "" && !ValidPaymentStatus(n.PaymentStatus) {
		errs.add("payment_status", "INVALID", "payment_status must be PENDING|PAID|REJECTED")
	}
	validateNRSParty(&errs, "accounting_supplier_party", n.AccountingSupplierParty, true)
	validateNRSParty(&errs, "accounting_customer_party", n.AccountingCustomerParty, false)
	if len(n.InvoiceLine) == 0 {
		errs.add("invoice_line", "REQUIRED", "at least one invoice line is required")
	}
	for i, l := range n.InvoiceLine {
		f := fmt.Sprintf("invoice_line[%d]", i)
		if strings.TrimSpace(l.Item.Name) == "" {
			errs.add(f+".item.name", "REQUIRED", "line item name is required")
		}
		if l.InvoicedQuantity <= 0 {
			errs.add(f+".invoiced_quantity", "INVALID", "invoiced_quantity must be > 0")
		}
		if l.Price.PriceAmount < 0 {
			errs.add(f+".price.price_amount", "INVALID", "price_amount must be >= 0")
		}
		if l.LineExtensionAmount < 0 {
			errs.add(f+".line_extension_amount", "INVALID", "line_extension_amount must be >= 0")
		}
		if l.HSNCode != "" && !ValidHSNCode(l.HSNCode) {
			errs.add(f+".hsn_code", "CATALOG", catalogError(f+".hsn_code", l.HSNCode, "hsn"))
		}
	}
	for i, tt := range n.TaxTotal {
		for j, st := range tt.TaxSubtotal {
			if _, ok := ValidTaxCategory(st.TaxCategory.ID); !ok {
				errs.add(fmt.Sprintf("tax_total[%d].tax_subtotal[%d].tax_category.id", i, j),
					"CATALOG", catalogError("tax_category.id", st.TaxCategory.ID, "tax-category"))
			}
		}
	}
	for i, pm := range n.PaymentMeans {
		if !ValidPaymentMeansCode(pm.PaymentMeansCode) {
			errs.add(fmt.Sprintf("payment_means[%d].payment_means_code", i),
				"CATALOG", catalogError("payment_means_code", pm.PaymentMeansCode, "payment-means"))
		}
	}
	if n.LegalMonetaryTotal.PayableAmount < 0 {
		errs.add("legal_monetary_total.payable_amount", "INVALID", "payable_amount must be >= 0")
	}
	return errs
}

func validateNRSParty(errs *NRSErrors, field string, p NRSParty, requireTIN bool) {
	if strings.TrimSpace(p.PartyName) == "" {
		errs.add(field+".party_name", "REQUIRED", field+" party_name is required")
	}
	if requireTIN && strings.TrimSpace(p.TIN) == "" {
		errs.add(field+".tin", "REQUIRED", field+" tin is required")
	}
	addr := p.PostalAddress
	if addr.State != "" && !ValidStateCode(addr.State) {
		errs.add(field+".postal_address.state", "CATALOG", catalogError(field+".postal_address.state", addr.State, "state"))
	}
	if addr.LGA != "" && !ValidLGACode(addr.LGA) {
		errs.add(field+".postal_address.lga", "CATALOG", catalogError(field+".postal_address.lga", addr.LGA, "lga"))
	}
}

// firstTaxCategory returns the first declared tax subtotal category, if any.
func (n *NRSInvoice) firstTaxCategory() (TaxCategory, bool) {
	for _, tt := range n.TaxTotal {
		for _, st := range tt.TaxSubtotal {
			if c, ok := ValidTaxCategory(st.TaxCategory.ID); ok {
				return c, true
			}
		}
	}
	return TaxCategories["STANDARD_VAT"], true
}

// FromNRS converts an NRS payload to the canonical model, converting all
// decimal NGN amounts to integer kobo with round-half-up. The payload must
// have passed Validate first (schema validation is a fail-fast gate).
func FromNRS(n *NRSInvoice) (*CanonicalInvoice, error) {
	if errs := n.Validate(); len(errs) > 0 {
		return nil, errs
	}
	cat, _ := n.firstTaxCategory()
	inv := &CanonicalInvoice{
		BusinessID:      n.BusinessID,
		IRN:             strings.TrimSpace(n.IRN),
		InvoiceNumber:   n.BuyerReference, // replaced below when empty
		InvoiceType:     "B2B",
		InvoiceTypeCode: n.InvoiceTypeCode,
		IssueDate:       n.IssueDate,
		DueDate:         n.DueDate,
		Currency:        strings.ToUpper(n.DocumentCurrencyCode),
		PaymentStatus:   strings.ToUpper(n.PaymentStatus),
		Supplier:        partyFromNRS(n.AccountingSupplierParty),
		Customer:        partyFromNRS(n.AccountingCustomerParty),
		SourceAdapter:   "nrs-schema",
	}
	if inv.PaymentStatus == "" {
		inv.PaymentStatus = "PENDING"
	}
	// Invoice number: prefer the number embedded in a supplied IRN, else
	// buyer_reference, else order_reference.
	if inv.IRN != "" {
		if num, _, _, err := ParseIRN(inv.IRN); err == nil {
			inv.InvoiceNumber = num
		}
	}
	if inv.InvoiceNumber == "" {
		inv.InvoiceNumber = n.OrderReference
	}
	if inv.InvoiceNumber == "" {
		return nil, NRSErrors{{Field: "irn|buyer_reference", Code: "REQUIRED",
			Message: "invoice number required: supply an irn or buyer_reference"}}
	}
	var excl, tax int64
	for i, l := range n.InvoiceLine {
		line := InvoiceLine{
			ID:            fmt.Sprintf("%d", i+1),
			Description:   l.Item.Name,
			QuantityMilli: int64(math.Round(l.InvoicedQuantity * 1000)),
			UnitPriceKobo: NGNToKobo(l.Price.PriceAmount),
			VatCategory:   cat.Canonical,
			VatRateBps:    cat.RateBps,
		}
		if l.Item.Description != "" {
			line.Description = l.Item.Name + " — " + l.Item.Description
		}
		if l.HSNCode != "" {
			line.Description = "[" + l.HSNCode + "] " + line.Description
		}
		line.LineTotalKobo = NGNToKobo(l.LineExtensionAmount)
		if line.LineTotalKobo == 0 {
			// derive: qty*price - discount + fee (all round-half-up at the boundary)
			gross := line.QuantityMilli * line.UnitPriceKobo / 1000
			line.LineTotalKobo = gross - NGNToKobo(l.DiscountAmount) + NGNToKobo(l.FeeAmount)
		}
		line.VatAmountKobo = RoundBpsHalfUp(line.LineTotalKobo, line.VatRateBps)
		excl += line.LineTotalKobo
		tax += line.VatAmountKobo
		inv.Lines = append(inv.Lines, line)
	}
	// document-level allowance/charge folds into the first line's totals path
	// via the declared legal_monetary_total below; we never store floats.
	inv.TaxExclusiveKobo = NGNToKobo(n.LegalMonetaryTotal.TaxExclusiveAmount)
	if inv.TaxExclusiveKobo == 0 {
		inv.TaxExclusiveKobo = excl
	}
	for _, tt := range n.TaxTotal {
		if k := NGNToKobo(tt.TaxAmount); k != 0 {
			inv.TaxKobo = k
			break
		}
	}
	if inv.TaxKobo == 0 {
		inv.TaxKobo = tax
	}
	inv.PayableKobo = NGNToKobo(n.LegalMonetaryTotal.PayableAmount)
	if inv.PayableKobo == 0 {
		inv.PayableKobo = inv.TaxExclusiveKobo + inv.TaxKobo
	}
	if inv.Currency == "" {
		inv.Currency = "NGN"
	}
	inv.Status = "received"
	return inv, nil
}

func partyFromNRS(p NRSParty) Party {
	return Party{
		TIN:     p.TIN,
		Name:    p.PartyName,
		Email:   p.Email,
		Address: p.PostalAddress.StreetName,
		City:    p.PostalAddress.CityName,
		State:   p.PostalAddress.State,
		Country: p.PostalAddress.Country,
	}
}

// ToNRS renders a canonical invoice back into the NRS schema for responses
// (kobo → decimal NGN at the boundary).
func ToNRS(inv *CanonicalInvoice) *NRSInvoice {
	n := &NRSInvoice{
		BusinessID:           inv.BusinessID,
		IRN:                  inv.IRN,
		IssueDate:            inv.IssueDate,
		DueDate:              inv.DueDate,
		InvoiceTypeCode:      inv.InvoiceTypeCode,
		DocumentCurrencyCode: inv.Currency,
		InvoiceKind:          inv.InvoiceType,
		PaymentStatus:        inv.PaymentStatus,
		AccountingSupplierParty: NRSParty{
			PartyName: inv.Supplier.Name, TIN: inv.Supplier.TIN, Email: inv.Supplier.Email,
			PostalAddress: NRSPostalAddress{
				StreetName: inv.Supplier.Address, CityName: inv.Supplier.City,
				State: inv.Supplier.State, Country: inv.Supplier.Country,
			},
		},
		AccountingCustomerParty: NRSParty{
			PartyName: inv.Customer.Name, TIN: inv.Customer.TIN, Email: inv.Customer.Email,
			PostalAddress: NRSPostalAddress{
				StreetName: inv.Customer.Address, CityName: inv.Customer.City,
				State: inv.Customer.State, Country: inv.Customer.Country,
			},
		},
	}
	if n.InvoiceTypeCode == "" {
		n.InvoiceTypeCode = "380"
	}
	var excl, tax int64
	for _, l := range inv.Lines {
		name := l.Description
		n.InvoiceLine = append(n.InvoiceLine, NRSInvoiceLine{
			InvoicedQuantity:    float64(l.QuantityMilli) / 1000,
			LineExtensionAmount: KoboToNGN(l.LineTotalKobo),
			Item:                NRSItem{Name: name},
			Price:               NRSPrice{PriceAmount: KoboToNGN(l.UnitPriceKobo), BaseQuantity: 1},
		})
		excl += l.LineTotalKobo
		tax += l.VatAmountKobo
	}
	if len(inv.Lines) > 0 {
		cat := TaxCategories["STANDARD_VAT"]
		switch inv.Lines[0].VatCategory {
		case "Z":
			cat = TaxCategories["ZERO_VAT"]
		case "E":
			cat = TaxCategories["EXEMPT"]
		}
		n.TaxTotal = []NRSTaxTotal{{
			TaxAmount: KoboToNGN(tax),
			TaxSubtotal: []NRSTaxSubtotal{{
				TaxableAmount: KoboToNGN(excl),
				TaxAmount:     KoboToNGN(tax),
				TaxCategory:   NRSTaxCategory{ID: cat.ID, Percent: float64(cat.RateBps) / 100},
			}},
		}}
	}
	n.LegalMonetaryTotal = NRSLegalMonetaryTotal{
		LineExtensionAmount: KoboToNGN(excl),
		TaxExclusiveAmount:  KoboToNGN(inv.TaxExclusiveKobo),
		TaxInclusiveAmount:  KoboToNGN(inv.TaxExclusiveKobo + inv.TaxKobo),
		PayableAmount:       KoboToNGN(inv.PayableKobo),
	}
	return n
}
