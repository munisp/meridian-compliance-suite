package main

// NRS reference catalogs (SPEC §5) used by schema validation:
// tax categories, invoice type codes, payment means codes, Nigerian state
// codes (ISO 3166-2:NG), LGA codes, ISO 4217 currencies and representative
// HSN/service product codes.
//
// Sources: einvoice.firs.gov.ng reference endpoints (tax-categories,
// invoice-types, payment-means), ISO 3166-2:NG, ISO 4217, WCO HS codes.
// Extension path: each catalog is a package-level map; append entries here
// (or wire the HTTP loaders behind the same Validate helpers) as NRS
// publishes new reference data — validators read the maps at call time.

import (
	"fmt"
	"regexp"
	"strings"
)

// TaxCategory is an NRS/FIRS tax category with its VAT rate in basis points.
type TaxCategory struct {
	ID      string `json:"id"`       // e.g. STANDARD_VAT
	RateBps int64  `json:"rate_bps"` // 750 = 7.5%
	// Canonical mapping into the internal kobo model VatCategory codes.
	Canonical string `json:"canonical"` // S|Z|E
	Label     string `json:"label"`
}

// TaxCategories: per einvoice.firs.gov.ng tax-categories.
var TaxCategories = map[string]TaxCategory{
	"STANDARD_VAT": {ID: "STANDARD_VAT", RateBps: 750, Canonical: "S", Label: "Standard VAT 7.5%"},
	"ZERO_VAT":     {ID: "ZERO_VAT", RateBps: 0, Canonical: "Z", Label: "Zero-rated VAT 0%"},
	"EXEMPT":       {ID: "EXEMPT", RateBps: 0, Canonical: "E", Label: "VAT exempt"},
	"VAT_EXEMPT":   {ID: "VAT_EXEMPT", RateBps: 0, Canonical: "E", Label: "VAT exempt (alias)"},
	"NON_VAT":      {ID: "NON_VAT", RateBps: 0, Canonical: "E", Label: "Outside scope of VAT"},
}

// ValidTaxCategory resolves an NRS tax category id (case-insensitive).
func ValidTaxCategory(id string) (TaxCategory, bool) {
	c, ok := TaxCategories[strings.ToUpper(strings.TrimSpace(id))]
	return c, ok
}

// InvoiceTypeCodes: UNTDID 1001 subset used by NRS (380 commercial invoice,
// 381 credit note, 383 debit note, ...).
var InvoiceTypeCodes = map[string]string{
	"380": "Commercial invoice",
	"381": "Credit note",
	"383": "Debit note",
	"384": "Corrected invoice",
	"386": "Prepayment invoice",
	"389": "Self-billed invoice",
	"751": "Invoice information for accounting purposes",
}

func ValidInvoiceTypeCode(code string) bool {
	_, ok := InvoiceTypeCodes[strings.TrimSpace(code)]
	return ok
}

// PaymentMeansCodes: UNTDID 4461 subset used by NRS.
var PaymentMeansCodes = map[string]string{
	"1":  "Instrument not defined",
	"10": "In cash",
	"20": "Cheque",
	"30": "Credit transfer",
	"31": "Debit transfer",
	"42": "Payment to bank account",
	"48": "Bank card",
	"49": "Direct debit",
	"97": "Clearing between partners",
}

func ValidPaymentMeansCode(code string) bool {
	_, ok := PaymentMeansCodes[strings.TrimSpace(code)]
	return ok
}

// StateCodes: ISO 3166-2:NG — 36 states + FCT.
var StateCodes = map[string]string{
	"NG-AB": "Abia", "NG-AD": "Adamawa", "NG-AK": "Akwa Ibom", "NG-AN": "Anambra",
	"NG-BA": "Bauchi", "NG-BY": "Bayelsa", "NG-BE": "Benue", "NG-BO": "Borno",
	"NG-CR": "Cross River", "NG-DE": "Delta", "NG-EB": "Ebonyi", "NG-ED": "Edo",
	"NG-EK": "Ekiti", "NG-EN": "Enugu", "NG-FC": "Federal Capital Territory",
	"NG-GO": "Gombe", "NG-IM": "Imo", "NG-JI": "Jigawa", "NG-KD": "Kaduna",
	"NG-KN": "Kano", "NG-KT": "Katsina", "NG-KE": "Kebbi", "NG-KO": "Kogi",
	"NG-KW": "Kwara", "NG-LA": "Lagos", "NG-NA": "Nasarawa", "NG-NI": "Niger",
	"NG-OG": "Ogun", "NG-ON": "Ondo", "NG-OS": "Osun", "NG-OY": "Oyo",
	"NG-PL": "Plateau", "NG-RI": "Rivers", "NG-SO": "Sokoto", "NG-TA": "Taraba",
	"NG-YO": "Yobe", "NG-ZA": "Zamfara",
}

func ValidStateCode(code string) bool {
	_, ok := StateCodes[strings.ToUpper(strings.TrimSpace(code))]
	return ok
}

// LGACodes: representative full sets (Abia, Lagos, FCT) in the NRS
// NG-<state>-<3 letters> convention (e.g. NG-AB-ANO). Extension path: append
// the remaining states' LGAs here following the same convention.
var LGACodes = map[string]string{
	// Abia (17)
	"NG-AB-ANO": "Aba North", "NG-AB-ASO": "Aba South", "NG-AB-ARN": "Arochukwu",
	"NG-AB-BEN": "Bende", "NG-AB-IKW": "Ikwuano", "NG-AB-IAL": "Isiala Ngwa North",
	"NG-AB-IAS": "Isiala Ngwa South", "NG-AB-ISU": "Isuikwuato", "NG-AB-OBN": "Obi Ngwa",
	"NG-AB-OHA": "Ohafia", "NG-AB-OSF": "Osisioma", "NG-AB-UGW": "Ugwunagbo",
	"NG-AB-UKW": "Ukwa East", "NG-AB-UKE": "Ukwa West", "NG-AB-UMN": "Umuahia North",
	"NG-AB-UMS": "Umuahia South", "NG-AB-UNN": "Umu Nneochi",
	// Lagos (20)
	"NG-LA-AGE": "Agege", "NG-LA-AJE": "Ajeromi-Ifelodun", "NG-LA-ALI": "Alimosho",
	"NG-LA-AMU": "Amuwo-Odofin", "NG-LA-APA": "Apapa", "NG-LA-BAD": "Badagry",
	"NG-LA-EPE": "Epe", "NG-LA-ETI": "Eti-Osa", "NG-LA-IBE": "Ibeju-Lekki",
	"NG-LA-IFK": "Ifako-Ijaiye", "NG-LA-IKE": "Ikeja", "NG-LA-IKO": "Ikorodu",
	"NG-LA-KOS": "Kosofe", "NG-LA-LAI": "Lagos Island", "NG-LA-LAM": "Lagos Mainland",
	"NG-LA-MUS": "Mushin", "NG-LA-OJO": "Ojo", "NG-LA-OSH": "Oshodi-Isolo",
	"NG-LA-SOM": "Somolu", "NG-LA-SUR": "Surulere",
	// FCT (6)
	"NG-FC-ABC": "Abaji", "NG-FC-AMC": "Abuja Municipal", "NG-FC-BWA": "Bwari",
	"NG-FC-GWA": "Gwagwalada", "NG-FC-KUJ": "Kuje", "NG-FC-KWL": "Kwali",
}

// lgaPattern enforces NG-<2 letters>-<3 letters> even for not-yet-catalogued LGAs.
var lgaPattern = regexp.MustCompile(`^NG-[A-Z]{2}-[A-Z]{3}$`)

// ValidLGACode accepts catalogued LGAs outright and structurally valid
// NG-XX-YYY codes whose state segment is a known state (documented extension
// path above).
func ValidLGACode(code string) bool {
	code = strings.ToUpper(strings.TrimSpace(code))
	if _, ok := LGACodes[code]; ok {
		return true
	}
	if !lgaPattern.MatchString(code) {
		return false
	}
	return ValidStateCode(code[:5])
}

// Currencies: ISO 4217 subset commonly seen on NRS invoices (NGN first).
var Currencies = map[string]bool{
	"NGN": true, "USD": true, "EUR": true, "GBP": true, "GHS": true,
	"KES": true, "ZAR": true, "XOF": true, "XAF": true, "CNY": true,
	"JPY": true, "CHF": true, "CAD": true, "AED": true, "SAR": true,
}

func ValidCurrency(code string) bool {
	return Currencies[strings.ToUpper(strings.TrimSpace(code))]
}

// hsnPattern: WCO HS codes / NRS service codes are 4-8 digits.
var hsnPattern = regexp.MustCompile(`^[0-9]{4,8}$`)

// HSNCodes: representative known product/service codes with descriptions.
var HSNCodes = map[string]string{
	"2523":   "Portland cement",
	"1006":   "Rice",
	"2710":   "Petroleum oils (non-crude)",
	"8517":   "Telephone sets / communication apparatus",
	"8471":   "Automatic data processing machines",
	"998314": "Information technology design services",
	"995411": "Construction services of buildings",
}

// ValidHSNCode validates structure; catalogued codes are preferred but any
// well-formed 4-8 digit code is accepted (extension path above).
func ValidHSNCode(code string) bool {
	return hsnPattern.MatchString(strings.TrimSpace(code))
}

// PaymentStatuses: NRS payment lifecycle enum.
var PaymentStatuses = map[string]bool{"PENDING": true, "PAID": true, "REJECTED": true}

func ValidPaymentStatus(s string) bool {
	return PaymentStatuses[strings.ToUpper(strings.TrimSpace(s))]
}

// catalogError is a small helper for descriptive catalog violations.
func catalogError(field, value, catalog string) string {
	return fmt.Sprintf("%s: value %q not in %s catalog", field, value, catalog)
}
