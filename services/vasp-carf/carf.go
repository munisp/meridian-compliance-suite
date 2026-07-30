package main

import (
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ---------- OECD CARF XML message model (structured subset of CARF v1) ----------

type CARFMessage struct {
	XMLName  xml.Name       `xml:"CARF_OECD"`
	Xmlns    string         `xml:"xmlns,attr"`
	Version  string         `xml:"version,attr"`
	Spec     CARFMessageSpec `xml:"MessageSpec"`
	Body     CARFBody       `xml:"CARFBody"`
}

type CARFMessageSpec struct {
	SendingCompanyIN   string `xml:"SendingCompanyIN"`
	TransmittingCountry string `xml:"TransmittingCountry"`
	ReceivingCountry   string `xml:"ReceivingCountry"`
	MessageType        string `xml:"MessageType"`
	MessageRefId       string `xml:"MessageRefId"`
	ReportingPeriod    string `xml:"ReportingPeriod"`
	Timestamp          string `xml:"Timestamp"`
}

type CARFBody struct {
	DocSpec       *CARFDocSpec  `xml:"DocSpec,omitempty"`
	ReportingVASP CARFVASP      `xml:"ReportingVASP"`
}

type CARFDocSpec struct {
	DocTypeIndic    string `xml:"DocTypeIndic"` // OECD1 new | OECD2 correction | OECD3 deletion
	CorrMessageRefId string `xml:"CorrMessageRefId,omitempty"`
}

type CARFVASP struct {
	Name    string     `xml:"VASPDetails>Name"`
	TINHash string     `xml:"VASPDetails>TINHash"`
	Country string     `xml:"VASPDetails>Country"`
	Reports []UserReport `xml:"UserReport"`
}

type UserReport struct {
	UserHash     string                 `xml:"ReportableUser>UserHash"`
	TINHash      string                 `xml:"ReportableUser>TINHash"`
	Country      string                 `xml:"ReportableUser>Country"`
	Transactions []CryptoAssetTransaction `xml:"CryptoAssetTransaction"`
}

type CryptoAssetTransaction struct {
	Type            string `xml:"Type"` // RC701|RC702|RC703|RC704
	Asset           string `xml:"CryptoAsset"`
	QtyMilli        int64  `xml:"QtyMilli"`
	FairMarketValue int64  `xml:"FairMarketValue"`
	Currency        string `xml:"Currency,attr"`
	GainLossKobo    int64  `xml:"GainLossKobo,omitempty"`
}

// ---------- message registry ----------

type CARFRecord struct {
	ID             string `json:"id"`
	MessageRefId   string `json:"message_ref_id"`
	Period         string `json:"period"`
	TenantID       string `json:"tenant_id"`
	DocTypeIndic   string `json:"doc_type_indic"`
	CorrOf         string `json:"corr_of,omitempty"`
	CorrectionReason string `json:"correction_reason,omitempty"`
	Users          int    `json:"users"`
	Transactions   int    `json:"transactions"`
	BuiltAt        string `json:"built_at"`
	Status         string `json:"status"` // built|transmitted|refused|superseded
	XML            string `json:"xml"`
	Validation     []string `json:"validation"`
}

type CARFStore struct {
	mu      sync.Mutex
	records map[string]*CARFRecord
}

func NewCARFStore() *CARFStore { return &CARFStore{records: map[string]*CARFRecord{}} }

func (cs *CARFStore) Add(r *CARFRecord) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.records[r.ID] = r
}

func (cs *CARFStore) Get(id string) (*CARFRecord, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	r, ok := cs.records[id]
	return r, ok
}

func (cs *CARFStore) List() []*CARFRecord {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := []*CARFRecord{}
	for _, r := range cs.records {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BuiltAt < out[j].BuiltAt })
	return out
}

// BuildCARFMessage constructs the CARF XML for a tenant+period from the
// engine ledger + trades. typeOf maps side/direction to RC70x codes.
func BuildCARFMessage(svc *Service, tenant, period, docType, corrOf, reason string) (*CARFRecord, error) {
	vaspName := tenant
	users := map[string]*UserReport{}
	txCount := 0
	for _, t := range svc.engine.Trades(tenant) {
		if period != "" && !strings.HasPrefix(t.TradedAt, period) {
			continue
		}
		ur := users[t.UserHash]
		if ur == nil {
			ur = &UserReport{UserHash: t.UserHash, TINHash: TINHash(t.UserHash, "carf-user"), Country: "NG"}
			users[t.UserHash] = ur
		}
		code := "RC701" // fiat-crypto exchange
		if t.Side == "sell" {
			code = "RC701"
		}
		ur.Transactions = append(ur.Transactions, CryptoAssetTransaction{
			Type: code, Asset: strings.ToUpper(t.Asset), QtyMilli: t.QtyMilli,
			FairMarketValue: amountKobo(t.QtyMilli, t.PriceKobo), Currency: "NGN",
		})
		txCount++
	}
	for _, tr := range svc.engine.Transfers(tenant) {
		if period != "" && !strings.HasPrefix(tr.MovedAt, period) {
			continue
		}
		ur := users[tr.UserHash]
		if ur == nil {
			ur = &UserReport{UserHash: tr.UserHash, TINHash: TINHash(tr.UserHash, "carf-user"), Country: "NG"}
			users[tr.UserHash] = ur
		}
		code := "RC703" // transfer
		ur.Transactions = append(ur.Transactions, CryptoAssetTransaction{
			Type: code, Asset: strings.ToUpper(tr.Asset), QtyMilli: tr.QtyMilli,
			FairMarketValue: amountKobo(tr.QtyMilli, tr.FMVKobo), Currency: "NGN",
		})
		txCount++
	}
	// attach gain/loss per user from accounting ledger (per-asset)
	for _, gl := range svc.engine.Ledger(tenant) {
		if period != "" && !strings.HasPrefix(gl.BookedAt, period) {
			continue
		}
		if ur := users[gl.UserHash]; ur != nil {
			ur.Transactions = append(ur.Transactions, CryptoAssetTransaction{
				Type: "RC704", Asset: gl.Asset, QtyMilli: 0,
				FairMarketValue: gl.Proceeds, Currency: "NGN", GainLossKobo: gl.GainLoss,
			})
			txCount++
		}
	}
	reports := []UserReport{}
	keys := []string{}
	for k := range users {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		reports = append(reports, *users[k])
	}
	rec := &CARFRecord{
		ID: "carf-" + ULID(), MessageRefId: "NG-" + ULID(), Period: period, TenantID: tenant,
		DocTypeIndic: docType, CorrOf: corrOf, CorrectionReason: reason,
		Users: len(reports), Transactions: txCount, BuiltAt: nowRFC3339(), Status: "built",
	}
	msg := CARFMessage{
		Xmlns: "urn:oecd:ties:carf:v1", Version: "1.1",
		Spec: CARFMessageSpec{
			SendingCompanyIN: TINHash(tenant, "carf-vasp-in"), TransmittingCountry: "NG",
			ReceivingCountry: "NG", MessageType: "CARF", MessageRefId: rec.MessageRefId,
			ReportingPeriod: period + "-12-31", Timestamp: rec.BuiltAt,
		},
		Body: CARFBody{
			ReportingVASP: CARFVASP{Name: vaspName, TINHash: TINHash(tenant, "carf-vasp"), Country: "NG", Reports: reports},
		},
	}
	if docType != "OECD1" {
		msg.Body.DocSpec = &CARFDocSpec{DocTypeIndic: docType, CorrMessageRefId: corrOf}
	}
	out, err := xml.MarshalIndent(msg, "", "  ")
	if err != nil {
		return nil, err
	}
	rec.XML = xml.Header + string(out)
	rec.Validation = ValidateCARF(rec, svc.packs)
	return rec, nil
}

// ValidateCARF checks the built message against rp-carf-schema required fields.
func ValidateCARF(rec *CARFRecord, packs *PackSet) []string {
	problems := []string{}
	required := packs.CARFRequiredFields()
	present := map[string]bool{
		"message_ref_id": rec.MessageRefId != "",
		"timestamp":      rec.BuiltAt != "",
		"reporting_vasp": rec.TenantID != "",
		"reportable_user": rec.Users > 0,
		"transactions":   rec.Transactions > 0,
	}
	for _, f := range required {
		if !present[f] {
			problems = append(problems, fmt.Sprintf("missing required field: %s", f))
		}
	}
	if !strings.Contains(rec.XML, "<MessageType>CARF</MessageType>") {
		problems = append(problems, "MessageType must be CARF")
	}
	if rec.DocTypeIndic != "OECD1" && rec.CorrOf == "" {
		problems = append(problems, "correction message missing CorrMessageRefId")
	}
	return problems
}
