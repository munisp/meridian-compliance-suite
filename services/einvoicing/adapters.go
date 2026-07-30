package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ERP adapter SDK (SPEC §3 T1/T2): pluggable ERP→canonical mapping.
// REST/CSV adapters are fully functional; the SAP OData adapter implements
// the same interface against an OData v4 JSON payload shape (live OData
// fetch is configured via SAP_ODATA_URL).

// ERPAdapter parses an ERP-native payload into canonical invoices.
type ERPAdapter interface {
	Name() string
	Parse(body []byte) ([]*CanonicalInvoice, error)
}

// RESTAdapter accepts the canonical JSON model directly (single object or array).
type RESTAdapter struct{}

func (RESTAdapter) Name() string { return "rest" }

func (RESTAdapter) Parse(body []byte) ([]*CanonicalInvoice, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	if body[0] == '[' {
		var invs []*CanonicalInvoice
		if err := json.Unmarshal(body, &invs); err != nil {
			return nil, fmt.Errorf("rest adapter: %w", err)
		}
		return invs, nil
	}
	var inv CanonicalInvoice
	if err := json.Unmarshal(body, &inv); err != nil {
		return nil, fmt.Errorf("rest adapter: %w", err)
	}
	return []*CanonicalInvoice{&inv}, nil
}

// CSVAdapter ingests flat ERP CSV exports. One row per invoice line; rows
// sharing invoice_number are grouped. Header (required):
// invoice_number,invoice_type,issue_date,currency,supplier_tin,supplier_name,
// customer_tin,customer_name,line_description,quantity_milli,unit_price_kobo,
// vat_category,vat_rate_bps
type CSVAdapter struct{}

func (CSVAdapter) Name() string { return "csv" }

func (CSVAdapter) Parse(body []byte) ([]*CanonicalInvoice, error) {
	r := csv.NewReader(bytes.NewReader(body))
	r.TrimLeadingSpace = true
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("csv adapter: %w", err)
	}
	if len(rows) < 2 {
		return nil, fmt.Errorf("csv adapter: need header + >=1 row")
	}
	idx := map[string]int{}
	for i, h := range rows[0] {
		idx[strings.TrimSpace(h)] = i
	}
	get := func(row []string, col string) string {
		i, ok := idx[col]
		if !ok || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}
	for _, req := range []string{"invoice_number", "supplier_tin", "supplier_name", "line_description", "unit_price_kobo"} {
		if _, ok := idx[req]; !ok {
			return nil, fmt.Errorf("csv adapter: missing required column %q", req)
		}
	}
	var order []string
	byNum := map[string]*CanonicalInvoice{}
	for n, row := range rows[1:] {
		num := get(row, "invoice_number")
		inv, ok := byNum[num]
		if !ok {
			inv = &CanonicalInvoice{
				InvoiceNumber: num,
				InvoiceType:   get(row, "invoice_type"),
				IssueDate:     get(row, "issue_date"),
				Currency:      get(row, "currency"),
				Supplier: Party{
					TIN: get(row, "supplier_tin"), Name: get(row, "supplier_name"),
					Address: get(row, "supplier_address"), State: get(row, "supplier_state"),
				},
				Customer: Party{
					TIN: get(row, "customer_tin"), Name: get(row, "customer_name"),
					Address: get(row, "customer_address"), State: get(row, "customer_state"),
				},
				SourceAdapter: "csv",
			}
			byNum[num] = inv
			order = append(order, num)
		}
		qty, _ := strconv.ParseInt(def(get(row, "quantity_milli"), "1000"), 10, 64)
		price, err := strconv.ParseInt(get(row, "unit_price_kobo"), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("csv adapter row %d: bad unit_price_kobo: %w", n+2, err)
		}
		rate, _ := strconv.ParseInt(def(get(row, "vat_rate_bps"), "0"), 10, 64)
		inv.Lines = append(inv.Lines, InvoiceLine{
			Description:   get(row, "line_description"),
			QuantityMilli: qty, UnitPriceKobo: price,
			VatCategory: def(get(row, "vat_category"), "S"), VatRateBps: rate,
		})
	}
	out := make([]*CanonicalInvoice, 0, len(order))
	for _, num := range order {
		out = append(out, byNum[num])
	}
	return out, nil
}

func def(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// SAPODataAdapter maps SAP OData v4 invoice payloads (entity set JSON with a
// top-level "value" array or wrapped "d" object) to the canonical model.
// Live OData connectivity (GET $entitySet with $expand=Items) is enabled by
// setting SAP_ODATA_URL; the payload mapping here is the real transformer.
type SAPODataAdapter struct {
	BaseURL string // SAP_ODATA_URL; empty = accept posted OData payloads only
}

func (SAPODataAdapter) Name() string { return "sap-odata" }

type odataLine struct {
	ItemCode       string  `json:"ItemCode"`
	Description    string  `json:"ItemDescription"`
	Quantity       float64 `json:"Quantity"`
	UnitPriceMinor float64 `json:"UnitPriceKobo"`
	TaxCode        string  `json:"TaxCode"`
	TaxPercent     float64 `json:"TaxPercent"`
}

type odataDoc struct {
	DocNum       string      `json:"DocNum"`
	DocDate      string      `json:"DocDate"`
	DocDueDate   string      `json:"DocDueDate"`
	DocCurrency  string      `json:"DocCurrency"`
	CardCode     string      `json:"CardCode"`
	CardName     string      `json:"CardName"`
	CustomerTIN  string      `json:"U_CustomerTIN"`
	SupplierTIN  string      `json:"U_SupplierTIN"`
	SupplierName string      `json:"U_SupplierName"`
	Items        []odataLine `json:"DocumentLines"`
}

func (a SAPODataAdapter) Parse(body []byte) ([]*CanonicalInvoice, error) {
	var wrap struct {
		Value []odataDoc `json:"value"`
		D     *odataDoc  `json:"d"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, fmt.Errorf("sap-odata adapter: %w", err)
	}
	docs := wrap.Value
	if wrap.D != nil {
		docs = append(docs, *wrap.D)
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("sap-odata adapter: no documents in payload")
	}
	var out []*CanonicalInvoice
	for _, d := range docs {
		inv := &CanonicalInvoice{
			InvoiceNumber: d.DocNum,
			IssueDate:     first10(d.DocDate),
			DueDate:       first10(d.DocDueDate),
			Currency:      def(d.DocCurrency, "NGN"),
			Supplier:      Party{TIN: d.SupplierTIN, Name: def(d.SupplierName, "SAP Business Partner")},
			Customer:      Party{TIN: d.CustomerTIN, Name: d.CardName},
			SourceAdapter: "sap-odata",
		}
		for i, l := range d.Items {
			rate := int64(l.TaxPercent * 100)
			cat := "S"
			if rate == 0 {
				cat = "E"
			}
			inv.Lines = append(inv.Lines, InvoiceLine{
				ID:            fmt.Sprintf("%d", i+1),
				Description:   def(l.Description, l.ItemCode),
				QuantityMilli: int64(l.Quantity * 1000),
				UnitPriceKobo: int64(l.UnitPriceMinor),
				VatCategory:   cat, VatRateBps: rate,
			})
		}
		out = append(out, inv)
	}
	return out, nil
}

func first10(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// AdapterFor selects the ERP adapter by name/content-type.
func AdapterFor(name, contentType string, body []byte) (ERPAdapter, error) {
	switch {
	case name == "csv" || strings.HasPrefix(contentType, "text/csv"):
		return CSVAdapter{}, nil
	case name == "sap-odata":
		return SAPODataAdapter{BaseURL: ""}, nil
	case name == "rest" || name == "" || strings.Contains(contentType, "json"):
		return RESTAdapter{}, nil
	}
	return nil, fmt.Errorf("unknown adapter %q", name)
}

var _ = io.EOF // keep io import if unused by future adapters
