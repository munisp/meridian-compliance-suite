package main

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// UBL 2.1 / Peppol BIS Billing 3.0 Invoice XML generation (SPEC §3 T1/T2).
// Real namespaced XML per the UBL 2.1 Invoice schema structure.

const (
	nsUBL = "urn:oasis:names:specification:ubl:schema:xsd:Invoice-2"
	nsCAC = "urn:oasis:names:specification:ubl:schema:xsd:CommonAggregateComponents-2"
	nsCBC = "urn:oasis:names:specification:ubl:schema:xsd:CommonBasicComponents-2"
	// Peppol BIS Billing 3.0 customisation/profile identifiers
	peppolCustomization = "urn:cen.eu:en16931:2017#compliant#urn:fdc:peppol.eu:2017:poacc:billing:3.0"
	peppolProfile       = "urn:fdc:peppol.eu:2017:poacc:billing:01:1.0"
)

type ublAmount struct {
	CurrencyID string `xml:"currencyID,attr"`
	Value      string `xml:",chardata"`
}

type ublPartyName struct {
	Name string `xml:"cbc:Name"`
}

type ublAddress struct {
	Street  string `xml:"cbc:StreetName,omitempty"`
	City    string `xml:"cbc:CityName,omitempty"`
	Region  string `xml:"cbc:CountrySubentity,omitempty"`
	Country struct {
		Code string `xml:"cbc:IdentificationCode"`
	} `xml:"cac:Country"`
}

type ublTaxScheme struct {
	ID string `xml:"cbc:ID"`
}

type ublPartyTaxScheme struct {
	CompanyID string       `xml:"cbc:CompanyID"` // TIN
	TaxScheme ublTaxScheme `xml:"cac:TaxScheme"`
}

type ublParty struct {
	PartyName    ublPartyName      `xml:"cac:PartyName"`
	Address      ublAddress        `xml:"cac:PostalAddress"`
	PartyTax     *ublPartyTaxScheme `xml:"cac:PartyTaxScheme,omitempty"`
}

type ublSupplierParty struct {
	Party ublParty `xml:"cac:Party"`
}

type ublCustomerParty struct {
	Party ublParty `xml:"cac:Party"`
}

type ublTaxSubtotal struct {
	TaxableAmount ublAmount    `xml:"cbc:TaxableAmount"`
	TaxAmount     ublAmount    `xml:"cbc:TaxAmount"`
	Category      ublTaxCat    `xml:"cac:TaxCategory"`
}

type ublTaxCat struct {
	ID        string       `xml:"cbc:ID"`
	Percent   string       `xml:"cbc:Percent"`
	TaxScheme ublTaxScheme `xml:"cac:TaxScheme"`
}

type ublTaxTotal struct {
	TaxAmount ublAmount       `xml:"cbc:TaxAmount"`
	Subtotals []ublTaxSubtotal `xml:"cac:TaxSubtotal"`
}

type ublMonetaryTotal struct {
	LineExtensionAmount ublAmount `xml:"cbc:LineExtensionAmount"`
	TaxExclusiveAmount  ublAmount `xml:"cbc:TaxExclusiveAmount"`
	TaxInclusiveAmount  ublAmount `xml:"cbc:TaxInclusiveAmount"`
	PayableAmount       ublAmount `xml:"cbc:PayableAmount"`
}

type ublPrice struct {
	Amount ublAmount `xml:"cbc:PriceAmount"`
}

type ublLineTaxTotal struct {
	TaxAmount ublAmount `xml:"cbc:TaxAmount"`
}

type ublItem struct {
	Description string `xml:"cbc:Description"`
	Name        string `xml:"cbc:Name"`
}

type ublQuantity struct {
	UnitCode string `xml:"unitCode,attr"`
	Value    string `xml:",chardata"`
}

type ublInvoiceLine struct {
	ID              string          `xml:"cbc:ID"`
	Quantity        ublQuantity     `xml:"cbc:InvoicedQuantity"`
	LineExtension   ublAmount       `xml:"cbc:LineExtensionAmount"`
	TaxTotal        ublLineTaxTotal `xml:"cac:TaxTotal"`
	Item            ublItem         `xml:"cac:Item"`
	Price           ublPrice        `xml:"cac:Price"`
}

// UBLInvoice is the root document.
type UBLInvoice struct {
	XMLName         xml.Name         `xml:"ubl:Invoice"`
	Xmlns           string           `xml:"xmlns,attr"`
	XmlnsCAC        string           `xml:"xmlns:cac,attr"`
	XmlnsCBC        string           `xml:"xmlns:cbc,attr"`
	CustomizationID string           `xml:"cbc:CustomizationID"`
	ProfileID       string           `xml:"cbc:ProfileID"`
	ID              string           `xml:"cbc:ID"`
	IssueDate       string           `xml:"cbc:IssueDate"`
	DueDate         string           `xml:"cbc:DueDate,omitempty"`
	InvoiceTypeCode string           `xml:"cbc:InvoiceTypeCode"`
	DocumentCurr    string           `xml:"cbc:DocumentCurrencyCode"`
	Supplier        ublSupplierParty `xml:"cac:AccountingSupplierParty"`
	Customer        ublCustomerParty `xml:"cac:AccountingCustomerParty"`
	TaxTotal        ublTaxTotal      `xml:"cac:TaxTotal"`
	MonetaryTotal   ublMonetaryTotal `xml:"cac:LegalMonetaryTotal"`
	Lines           []ublInvoiceLine `xml:"cac:InvoiceLine"`
}

// koboToNGN renders integer kobo as a decimal NGN string (e.g. 12345 -> "123.45").
func koboToNGN(k int64) string {
	sign := ""
	if k < 0 {
		sign = "-"
		k = -k
	}
	return fmt.Sprintf("%s%d.%02d", sign, k/100, k%100)
}

func amount(k int64, cur string) ublAmount { return ublAmount{CurrencyID: cur, Value: koboToNGN(k)} }

func mapParty(p Party) ublParty {
	up := ublParty{PartyName: ublPartyName{Name: p.Name}}
	up.Address.Street = p.Address
	up.Address.City = p.City
	up.Address.Region = p.State
	up.Address.Country.Code = p.Country
	if p.TIN != "" {
		up.PartyTax = &ublPartyTaxScheme{CompanyID: p.TIN, TaxScheme: ublTaxScheme{ID: "VAT"}}
	}
	return up
}

func milliToDecimal(m int64) string {
	sign := ""
	if m < 0 {
		sign = "-"
		m = -m
	}
	whole, frac := m/1000, m%1000
	s := fmt.Sprintf("%s%d", sign, whole)
	if frac != 0 {
		s += fmt.Sprintf(".%03d", frac)
		s = strings.TrimRight(s, "0")
	}
	return s
}

// MapUBL converts a canonical invoice to the UBL 2.1 document model.
func MapUBL(inv *CanonicalInvoice) *UBLInvoice {
	doc := &UBLInvoice{
		Xmlns: nsUBL, XmlnsCAC: nsCAC, XmlnsCBC: nsCBC,
		CustomizationID: peppolCustomization,
		ProfileID:       peppolProfile,
		ID:              inv.InvoiceNumber,
		IssueDate:       inv.IssueDate,
		DueDate:         inv.DueDate,
		InvoiceTypeCode: "380", // UNCL1001: commercial invoice
		DocumentCurr:    inv.Currency,
		Supplier:        ublSupplierParty{Party: mapParty(inv.Supplier)},
		Customer:        ublCustomerParty{Party: mapParty(inv.Customer)},
	}
	// Group VAT subtotals by category+rate.
	type key struct {
		cat  string
		rate int64
	}
	groups := map[key]*ublTaxSubtotal{}
	var order []key
	for _, l := range inv.Lines {
		k := key{l.VatCategory, l.VatRateBps}
		st, ok := groups[k]
		if !ok {
			st = &ublTaxSubtotal{Category: ublTaxCat{
				ID: l.VatCategory, Percent: fmt.Sprintf("%.2f", float64(l.VatRateBps)/100),
				TaxScheme: ublTaxScheme{ID: "VAT"},
			}}
			groups[k] = st
			order = append(order, k)
		}
		st.TaxableAmount = amount(mustKobo(st.TaxableAmount)+l.LineTotalKobo, inv.Currency)
		st.TaxAmount = amount(mustKobo(st.TaxAmount)+l.VatAmountKobo, inv.Currency)
	}
	doc.TaxTotal.TaxAmount = amount(inv.TaxKobo, inv.Currency)
	for _, k := range order {
		doc.TaxTotal.Subtotals = append(doc.TaxTotal.Subtotals, *groups[k])
	}
	doc.MonetaryTotal = ublMonetaryTotal{
		LineExtensionAmount: amount(inv.TaxExclusiveKobo, inv.Currency),
		TaxExclusiveAmount:  amount(inv.TaxExclusiveKobo, inv.Currency),
		TaxInclusiveAmount:  amount(inv.PayableKobo, inv.Currency),
		PayableAmount:       amount(inv.PayableKobo, inv.Currency),
	}
	for _, l := range inv.Lines {
		doc.Lines = append(doc.Lines, ublInvoiceLine{
			ID:            l.ID,
			Quantity:      ublQuantity{UnitCode: "EA", Value: milliToDecimal(l.QuantityMilli)},
			LineExtension: amount(l.LineTotalKobo, inv.Currency),
			TaxTotal:      ublLineTaxTotal{TaxAmount: amount(l.VatAmountKobo, inv.Currency)},
			Item:          ublItem{Description: l.Description, Name: l.Description},
			Price:         ublPrice{Amount: amount(l.UnitPriceKobo, inv.Currency)},
		})
	}
	return doc
}

func mustKobo(a ublAmount) int64 {
	if a.Value == "" {
		return 0
	}
	var n int64
	fmt.Sscanf(strings.ReplaceAll(a.Value, ".", ""), "%d", &n)
	return n
}

// GenerateUBL renders the canonical invoice as UBL 2.1 XML bytes.
func GenerateUBL(inv *CanonicalInvoice) ([]byte, error) {
	out, err := xml.MarshalIndent(MapUBL(inv), "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out...), nil
}
