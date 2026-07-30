package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// LedgerClient mirrors the core ledger svc contract (SPEC §1.5).
// Dev mode uses an in-memory TigerBeetle-semantics implementation so the
// service runs with zero external deps; real wiring via LEDGER_SVC_URL.
type LedgerClient interface {
	CreateAccounts(accts []LedgerAccount) error
	Transfer(t LedgerTransfer) (string, error)
	PendingTransfer(t LedgerTransfer) (string, error)
	PostPending(pendingID string, amountKobo int64) (string, error)
	VoidPending(pendingID string) error
	Balance(accountID string) (*LedgerBalance, error)
	Mode() string
}

// Account id = 128-bit: high 64 namespace code, low 64 entity serial (SPEC §1.5).
func accountID(namespace uint64, serial uint64) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[0:8], namespace)
	binary.BigEndian.PutUint64(b[8:16], serial)
	return fmt.Sprintf("%x", b)
}

const (
	LedgerVATRemittance = 300
	NSVATFederalPool    = 1
	NSVATStatePool      = 2
	NSVATMerchant       = 3
)

type LedgerAccount struct {
	ID     string `json:"id"`
	Ledger int    `json:"ledger"`
	Code   int    `json:"code"`
	Flags  string `json:"flags,omitempty"`
}

type LedgerTransfer struct {
	ID              string `json:"id,omitempty"`
	DebitAccountID  string `json:"debit_account_id"`
	CreditAccountID string `json:"credit_account_id"`
	AmountKobo      int64  `json:"amount_kobo"`
	Ledger          int    `json:"ledger"`
	Code            int    `json:"code"`
}

type LedgerBalance struct {
	AccountID      string `json:"account_id"`
	DebitsPosted   int64  `json:"debits_posted_kobo"`
	CreditsPosted  int64  `json:"credits_posted_kobo"`
	DebitsPending  int64  `json:"debits_pending_kobo"`
	CreditsPending int64  `json:"credits_pending_kobo"`
}

// ---------- HTTP client to core ledger svc ----------

type HTTPLedger struct{ Base string }

func (h *HTTPLedger) Mode() string { return "core" }

func (h *HTTPLedger) do(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.Base+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	cli := &http.Client{Timeout: 3 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("ledger %d: %s", resp.StatusCode, string(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (h *HTTPLedger) CreateAccounts(accts []LedgerAccount) error {
	return h.do("POST", "/v1/accounts", map[string]any{"accounts": accts}, nil)
}

func (h *HTTPLedger) Transfer(t LedgerTransfer) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := h.do("POST", "/v1/transfers", t, &out)
	if out.ID == "" {
		out.ID = t.ID
	}
	return out.ID, err
}

func (h *HTTPLedger) PendingTransfer(t LedgerTransfer) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := h.do("POST", "/v1/transfers/pending", t, &out)
	if out.ID == "" {
		out.ID = t.ID
	}
	return out.ID, err
}

func (h *HTTPLedger) PostPending(pendingID string, amount int64) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := h.do("POST", "/v1/transfers/"+pendingID+"/post", map[string]int64{"amount_kobo": amount}, &out)
	return pendingID, err
}

func (h *HTTPLedger) VoidPending(pendingID string) error {
	return h.do("POST", "/v1/transfers/"+pendingID+"/void", map[string]any{}, nil)
}

func (h *HTTPLedger) Balance(accountID string) (*LedgerBalance, error) {
	var out LedgerBalance
	err := h.do("GET", "/v1/accounts/"+accountID+"/balance", nil, &out)
	return &out, err
}

// ---------- Dev in-memory TigerBeetle-semantics ledger ----------

type DevLedger struct {
	mu        sync.Mutex
	accounts  map[string]*devAccount
	transfers map[string]*devTransfer
	seq       uint64
}

type devAccount struct {
	LedgerAccount
	debitsPosted, creditsPosted   int64
	debitsPending, creditsPending int64
}

type devTransfer struct {
	LedgerTransfer
	pending bool
	voided  bool
	posted  bool
}

func NewDevLedger() *DevLedger {
	dl := &DevLedger{accounts: map[string]*devAccount{}, transfers: map[string]*devTransfer{}}
	// seed VAT remittance ledger accounts (ledger 300)
	dl.CreateAccounts([]LedgerAccount{
		{ID: accountID(LedgerVATRemittance, NSVATFederalPool), Ledger: LedgerVATRemittance, Code: 5},
		{ID: accountID(LedgerVATRemittance, NSVATStatePool), Ledger: LedgerVATRemittance, Code: 5},
	})
	return dl
}

func (d *DevLedger) Mode() string { return "dev" }

func (d *DevLedger) CreateAccounts(accts []LedgerAccount) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range accts {
		if _, ok := d.accounts[a.ID]; !ok {
			cp := a
			d.accounts[a.ID] = &devAccount{LedgerAccount: cp}
		}
	}
	return nil
}

func (d *DevLedger) nextID() string {
	d.seq++
	return fmt.Sprintf("dev-tx-%d", d.seq)
}

func (d *DevLedger) Transfer(t LedgerTransfer) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkAccounts(t); err != nil {
		return "", err
	}
	if t.ID == "" {
		t.ID = d.nextID()
	}
	d.apply(t)
	d.transfers[t.ID] = &devTransfer{LedgerTransfer: t, posted: true}
	return t.ID, nil
}

func (d *DevLedger) PendingTransfer(t LedgerTransfer) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkAccounts(t); err != nil {
		return "", err
	}
	if t.ID == "" {
		t.ID = d.nextID()
	}
	dr, cr := d.accounts[t.DebitAccountID], d.accounts[t.CreditAccountID]
	dr.debitsPending += t.AmountKobo
	cr.creditsPending += t.AmountKobo
	d.transfers[t.ID] = &devTransfer{LedgerTransfer: t, pending: true}
	return t.ID, nil
}

func (d *DevLedger) PostPending(pendingID string, amount int64) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tr, ok := d.transfers[pendingID]
	if !ok || !tr.pending || tr.voided {
		return "", errors.New("pending transfer not found or not postable")
	}
	if amount <= 0 || amount > tr.AmountKobo {
		return "", errors.New("post amount must be <= pending amount")
	}
	dr, cr := d.accounts[tr.DebitAccountID], d.accounts[tr.CreditAccountID]
	dr.debitsPending -= tr.AmountKobo
	cr.creditsPending -= tr.AmountKobo
	post := tr.LedgerTransfer
	post.AmountKobo = amount
	if err := d.checkFloat(dr, amount); err != nil {
		return "", err
	}
	d.apply(post)
	tr.pending, tr.posted = false, true
	return pendingID, nil
}

func (d *DevLedger) VoidPending(pendingID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tr, ok := d.transfers[pendingID]
	if !ok || !tr.pending || tr.voided {
		return errors.New("pending transfer not found or not voidable")
	}
	dr, cr := d.accounts[tr.DebitAccountID], d.accounts[tr.CreditAccountID]
	dr.debitsPending -= tr.AmountKobo
	cr.creditsPending -= tr.AmountKobo
	tr.pending, tr.voided = false, true
	return nil
}

func (d *DevLedger) checkAccounts(t LedgerTransfer) error {
	if t.AmountKobo <= 0 {
		return errors.New("amount must be positive kobo")
	}
	if _, ok := d.accounts[t.DebitAccountID]; !ok {
		return fmt.Errorf("debit account %s missing", t.DebitAccountID)
	}
	if _, ok := d.accounts[t.CreditAccountID]; !ok {
		return fmt.Errorf("credit account %s missing", t.CreditAccountID)
	}
	return d.checkFloat(d.accounts[t.DebitAccountID], t.AmountKobo)
}

// DEBITS_MUST_NOT_EXCEED_CREDITS on float accounts (SPEC §1.5).
func (d *DevLedger) checkFloat(a *devAccount, amount int64) error {
	if a.Flags == "DEBITS_MUST_NOT_EXCEED_CREDITS" {
		if a.debitsPosted+amount > a.creditsPosted {
			return errors.New("float constraint violated: debits would exceed credits")
		}
	}
	return nil
}

func (d *DevLedger) apply(t LedgerTransfer) {
	d.accounts[t.DebitAccountID].debitsPosted += t.AmountKobo
	d.accounts[t.CreditAccountID].creditsPosted += t.AmountKobo
}

func (d *DevLedger) Balance(accountID string) (*LedgerBalance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a, ok := d.accounts[accountID]
	if !ok {
		return nil, errors.New("account not found")
	}
	return &LedgerBalance{
		AccountID: accountID, DebitsPosted: a.debitsPosted, CreditsPosted: a.creditsPosted,
		DebitsPending: a.debitsPending, CreditsPending: a.creditsPending,
	}, nil
}
