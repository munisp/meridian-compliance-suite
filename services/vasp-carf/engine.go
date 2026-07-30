package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Quantities are integer milli-asset-units; money is integer kobo (SPEC §1.3).

type Trade struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id"`
	VASPRef     string `json:"vasp_ref"`
	UserHash    string `json:"user_hash"` // pseudonymised user ref
	Asset       string `json:"asset"`     // e.g. BTC, ETH, USDT
	Side        string `json:"side"`      // buy|sell
	QtyMilli    int64  `json:"qty_milli"`
	PriceKobo   int64  `json:"price_kobo"` // per whole asset
	FeeKobo     int64  `json:"fee_kobo"`
	TradedAt    string `json:"traded_at"`
	TxHash      string `json:"tx_hash,omitempty"`
}

type Transfer struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	UserHash  string `json:"user_hash"`
	Asset     string `json:"asset"`
	Direction string `json:"direction"` // in|out
	QtyMilli  int64  `json:"qty_milli"`
	FMVKobo   int64  `json:"fmv_kobo"` // per whole asset at transfer time (0 -> FMV cache)
	MovedAt   string `json:"moved_at"`
}

// Lot is a FIFO cost-basis lot.
type Lot struct {
	QtyMilli   int64 `json:"qty_milli"`
	CostPerUnit int64 `json:"cost_per_unit_kobo"` // per whole asset
	AcquiredAt string `json:"acquired_at"`
}

// GainLossEntry = per-asset accounting ledger entry (NOT payments).
type GainLossEntry struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	UserHash  string `json:"user_hash"`
	Asset     string `json:"asset"`
	Kind      string `json:"kind"` // disposal|transfer-out-fmv|ringfence
	Proceeds  int64  `json:"proceeds_kobo"`
	Basis     int64  `json:"basis_kobo"`
	GainLoss  int64  `json:"gain_loss_kobo"` // + gain, - loss
	Method    string `json:"method"`         // fifo|wac
	TradeID   string `json:"trade_id,omitempty"`
	Memo      string `json:"memo"`
	BookedAt  string `json:"booked_at"`
}

type FMVSnapshot struct {
	Asset     string `json:"asset"`
	PriceKobo int64  `json:"price_kobo"`
	Source    string `json:"source"`
	At        string `json:"at"`
}

// Engine holds trades, basis lots, FMV cache, and the accounting ledger.
// Durable via embedded append-log (honesty tag: SQLite stand-in, stdlib-only build).
type Engine struct {
	mu       sync.Mutex
	dir      string
	trades   map[string]*Trade
	transfers map[string]*Transfer
	lots     map[string][]Lot // key tenant|user|asset (FIFO queue)
	wac      map[string]*wacState
	fmv      map[string][]FMVSnapshot // asset -> snapshots
	ledger   []GainLossEntry
	logFile  *os.File
}

type wacState struct {
	QtyMilli  int64 `json:"qty_milli"`
	CostTotal int64 `json:"cost_total_kobo"`
}

func key3(tenant, user, asset string) string {
	return tenant + "|" + user + "|" + strings.ToUpper(asset)
}

func NewEngine(dir string) *Engine {
	e := &Engine{
		dir: dir, trades: map[string]*Trade{}, transfers: map[string]*Transfer{},
		lots: map[string][]Lot{}, wac: map[string]*wacState{},
		fmv: map[string][]FMVSnapshot{},
	}
	os.MkdirAll(dir, 0o755)
	e.replay()
	f, err := os.OpenFile(filepath.Join(dir, "engine.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		e.logFile = f
	}
	return e
}

type logRec struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

func (e *Engine) append(kind string, v any) {
	if e.logFile == nil {
		return
	}
	b, _ := json.Marshal(logRec{Kind: kind, Data: json.RawMessage(mustJSON(v))})
	e.logFile.Write(append(b, '\n'))
	e.logFile.Sync()
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func (e *Engine) replay() {
	f, err := os.Open(filepath.Join(e.dir, "engine.log"))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var rec logRec
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		switch rec.Kind {
		case "trade":
			var t Trade
			if json.Unmarshal(rec.Data, &t) == nil {
				e.applyTrade(&t)
			}
		case "transfer":
			var tr Transfer
			if json.Unmarshal(rec.Data, &tr) == nil {
				e.applyTransfer(&tr)
			}
		case "fmv":
			var s FMVSnapshot
			if json.Unmarshal(rec.Data, &s) == nil {
				e.fmv[strings.ToUpper(s.Asset)] = append(e.fmv[strings.ToUpper(s.Asset)], s)
			}
		case "ledger":
			var gl GainLossEntry
			if json.Unmarshal(rec.Data, &gl) == nil {
				e.ledger = append(e.ledger, gl)
			}
		}
	}
}

func amountKobo(qtyMilli, pricePerUnit int64) int64 {
	// qty (milli) * price (kobo per whole asset) / 1000
	return qtyMilli * pricePerUnit / 1000
}

// IngestTrade records a trade and updates basis + gain/loss ledger on sells.
func (e *Engine) IngestTrade(t *Trade, method string) (*GainLossEntry, error) {
	if t.QtyMilli <= 0 || t.PriceKobo < 0 {
		return nil, fmt.Errorf("qty_milli must be > 0 and price_kobo >= 0")
	}
	if t.Side != "buy" && t.Side != "sell" {
		return nil, fmt.Errorf("side must be buy|sell")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if t.ID == "" {
		t.ID = "trd-" + ULID()
	}
	if _, dup := e.trades[t.ID]; dup {
		return nil, nil // idempotent
	}
	if t.TradedAt == "" {
		t.TradedAt = nowRFC3339()
	}
	var gl *GainLossEntry
	if t.Side == "sell" {
		entry, err := e.bookDisposal(t, method)
		if err != nil {
			return nil, err
		}
		gl = entry
	} else {
		e.acquire(t, t.PriceKobo)
	}
	e.trades[t.ID] = t
	e.append("trade", t)
	if gl != nil {
		e.ledger = append(e.ledger, *gl)
		e.append("ledger", gl)
	}
	return gl, nil
}

// applyTrade is replay-only (no logging).
func (e *Engine) applyTrade(t *Trade) {
	if _, ok := e.trades[t.ID]; ok {
		return
	}
	e.trades[t.ID] = t
	if t.Side == "buy" {
		e.acquire(t, t.PriceKobo)
	} else {
		e.consumeBasis(t.TenantID, t.UserHash, t.Asset, t.QtyMilli, "fifo")
		e.consumeWAC(t.TenantID, t.UserHash, t.Asset, t.QtyMilli)
	}
}

func (e *Engine) applyTransfer(tr *Transfer) {
	if _, ok := e.transfers[tr.ID]; ok {
		return
	}
	e.transfers[tr.ID] = tr
	if tr.Direction == "in" {
		e.acquire(&Trade{TenantID: tr.TenantID, UserHash: tr.UserHash, Asset: tr.Asset,
			QtyMilli: tr.QtyMilli, TradedAt: tr.MovedAt}, tr.FMVKobo)
	} else {
		e.consumeBasis(tr.TenantID, tr.UserHash, tr.Asset, tr.QtyMilli, "fifo")
		e.consumeWAC(tr.TenantID, tr.UserHash, tr.Asset, tr.QtyMilli)
	}
}

func (e *Engine) acquire(t *Trade, costPerUnit int64) {
	k := key3(t.TenantID, t.UserHash, t.Asset)
	e.lots[k] = append(e.lots[k], Lot{QtyMilli: t.QtyMilli, CostPerUnit: costPerUnit, AcquiredAt: t.TradedAt})
	w := e.wac[k]
	if w == nil {
		w = &wacState{}
		e.wac[k] = w
	}
	w.CostTotal += amountKobo(t.QtyMilli, costPerUnit)
	w.QtyMilli += t.QtyMilli
}

// bookDisposal computes basis relief for a sell and books the gain/loss entry.
func (e *Engine) bookDisposal(t *Trade, method string) (*GainLossEntry, error) {
	if method == "" {
		method = "fifo"
	}
	proceeds := amountKobo(t.QtyMilli, t.PriceKobo) - t.FeeKobo
	var basis int64
	switch method {
	case "fifo":
		b, err := e.consumeBasis(t.TenantID, t.UserHash, t.Asset, t.QtyMilli, "fifo")
		if err != nil {
			return nil, err
		}
		basis = b
	case "wac":
		b, err := e.consumeWAC(t.TenantID, t.UserHash, t.Asset, t.QtyMilli)
		if err != nil {
			return nil, err
		}
		basis = b
	default:
		return nil, fmt.Errorf("unknown method %q (fifo|wac)", method)
	}
	entry := &GainLossEntry{
		ID: "gl-" + ULID(), TenantID: t.TenantID, UserHash: t.UserHash, Asset: strings.ToUpper(t.Asset),
		Kind: "disposal", Proceeds: proceeds, Basis: basis, GainLoss: proceeds - basis,
		Method: method, TradeID: t.ID,
		Memo: fmt.Sprintf("disposal of %d milli-%s @ %d kobo/asset", t.QtyMilli, strings.ToUpper(t.Asset), t.PriceKobo),
		BookedAt: nowRFC3339(),
	}
	return entry, nil
}

// consumeBasis relieves FIFO lots; returns basis kobo.
func (e *Engine) consumeBasis(tenant, user, asset string, qtyMilli int64, method string) (int64, error) {
	k := key3(tenant, user, asset)
	lots := e.lots[k]
	var have int64
	for _, l := range lots {
		have += l.QtyMilli
	}
	if have < qtyMilli {
		return 0, fmt.Errorf("insufficient basis: have %d milli, need %d", have, qtyMilli)
	}
	remain := qtyMilli
	var basis int64
	for remain > 0 && len(lots) > 0 {
		head := lots[0]
		take := head.QtyMilli
		if take > remain {
			take = remain
		}
		basis += amountKobo(take, head.CostPerUnit)
		head.QtyMilli -= take
		remain -= take
		if head.QtyMilli == 0 {
			lots = lots[1:]
		} else {
			lots[0] = head
		}
	}
	e.lots[k] = lots
	return basis, nil
}

func (e *Engine) consumeWAC(tenant, user, asset string, qtyMilli int64) (int64, error) {
	k := key3(tenant, user, asset)
	w := e.wac[k]
	if w == nil || w.QtyMilli < qtyMilli {
		var have int64
		if w != nil {
			have = w.QtyMilli
		}
		return 0, fmt.Errorf("insufficient wac basis: have %d milli, need %d", have, qtyMilli)
	}
	// weighted average cost per milli-unit relief
	basis := w.CostTotal * qtyMilli / w.QtyMilli
	w.CostTotal -= basis
	w.QtyMilli -= qtyMilli
	return basis, nil
}

// IngestTransfer records a deposit (basis at FMV) or withdrawal (disposal at FMV).
func (e *Engine) IngestTransfer(tr *Transfer) (*GainLossEntry, error) {
	if tr.QtyMilli <= 0 {
		return nil, fmt.Errorf("qty_milli must be > 0")
	}
	if tr.Direction != "in" && tr.Direction != "out" {
		return nil, fmt.Errorf("direction must be in|out")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if tr.ID == "" {
		tr.ID = "xfr-" + ULID()
	}
	if _, dup := e.transfers[tr.ID]; dup {
		return nil, nil
	}
	if tr.MovedAt == "" {
		tr.MovedAt = nowRFC3339()
	}
	if tr.FMVKobo == 0 {
		if snap, ok := e.latestFMV(tr.Asset); ok {
			tr.FMVKobo = snap.PriceKobo
		}
	}
	var gl *GainLossEntry
	if tr.Direction == "in" {
		e.acquire(&Trade{TenantID: tr.TenantID, UserHash: tr.UserHash, Asset: tr.Asset,
			QtyMilli: tr.QtyMilli, TradedAt: tr.MovedAt}, tr.FMVKobo)
	} else {
		proceeds := amountKobo(tr.QtyMilli, tr.FMVKobo)
		basis, err := e.consumeBasis(tr.TenantID, tr.UserHash, tr.Asset, tr.QtyMilli, "fifo")
		if err != nil {
			return nil, err
		}
		e.consumeWAC(tr.TenantID, tr.UserHash, tr.Asset, tr.QtyMilli)
		gl = &GainLossEntry{
			ID: "gl-" + ULID(), TenantID: tr.TenantID, UserHash: tr.UserHash, Asset: strings.ToUpper(tr.Asset),
			Kind: "transfer-out-fmv", Proceeds: proceeds, Basis: basis, GainLoss: proceeds - basis,
			Method: "fifo", Memo: "withdrawal treated as disposal at FMV (NTA digital assets)",
			BookedAt: nowRFC3339(),
		}
		e.ledger = append(e.ledger, *gl)
		e.append("ledger", gl)
	}
	e.transfers[tr.ID] = tr
	e.append("transfer", tr)
	return gl, nil
}

// AddFMV caches a fair-market-value snapshot.
func (e *Engine) AddFMV(s FMVSnapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s.At == "" {
		s.At = nowRFC3339()
	}
	a := strings.ToUpper(s.Asset)
	e.fmv[a] = append(e.fmv[a], s)
	e.append("fmv", s)
}

func (e *Engine) latestFMV(asset string) (FMVSnapshot, bool) {
	a := strings.ToUpper(asset)
	snaps := e.fmv[a]
	if len(snaps) == 0 {
		return FMVSnapshot{}, false
	}
	return snaps[len(snaps)-1], true
}

func (e *Engine) LatestFMV(asset string) (FMVSnapshot, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.latestFMV(asset)
}

// CostBasis reports remaining position basis under fifo or wac.
func (e *Engine) CostBasis(tenant, user, asset, method string) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	k := key3(tenant, user, asset)
	out := map[string]any{"asset": strings.ToUpper(asset), "method": method}
	if method == "wac" {
		w := e.wac[k]
		if w == nil {
			out["qty_milli"] = int64(0)
			out["basis_kobo"] = int64(0)
			return out
		}
		out["qty_milli"] = w.QtyMilli
		out["basis_kobo"] = w.CostTotal
		if w.QtyMilli > 0 {
			out["avg_cost_kobo_per_asset"] = w.CostTotal * 1000 / w.QtyMilli
		}
		return out
	}
	var qty, basis int64
	lots := e.lots[k]
	for _, l := range lots {
		qty += l.QtyMilli
		basis += amountKobo(l.QtyMilli, l.CostPerUnit)
	}
	out["qty_milli"] = qty
	out["basis_kobo"] = basis
	out["open_lots"] = len(lots)
	out["lots"] = lots
	return out
}

func (e *Engine) Ledger(tenant string) []GainLossEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []GainLossEntry{}
	for _, gl := range e.ledger {
		if tenant == "" || gl.TenantID == tenant {
			out = append(out, gl)
		}
	}
	return out
}

func (e *Engine) Trades(tenant string) []*Trade {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []*Trade{}
	for _, t := range e.trades {
		if tenant == "" || t.TenantID == tenant {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TradedAt < out[j].TradedAt })
	return out
}

func (e *Engine) Transfers(tenant string) []*Transfer {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []*Transfer{}
	for _, tr := range e.transfers {
		if tenant == "" || tr.TenantID == tenant {
			out = append(out, tr)
		}
	}
	return out
}

// RingFence computes the ring-fenced position per rp-nta-digital-assets:
// digital-asset losses may only offset digital-asset gains of the same
// user in the same period; excess losses carry forward (not sideways).
type RingFenceResult struct {
	TenantID      string         `json:"tenant_id"`
	UserHash      string         `json:"user_hash"`
	Period        string         `json:"period"`
	GrossGains    int64          `json:"gross_gains_kobo"`
	GrossLosses   int64          `json:"gross_losses_kobo"`
	NetRingFenced int64          `json:"net_ring_fenced_kobo"`
	TaxableGain   int64          `json:"taxable_gain_kobo"`
	CarryForward  int64          `json:"carry_forward_loss_kobo"`
	PerAsset      map[string]int64 `json:"per_asset_gain_loss_kobo"`
	RuleRef       string         `json:"rule_ref"`
}

func (e *Engine) RingFence(tenant, user, period string, pack *Pack) RingFenceResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	res := RingFenceResult{TenantID: tenant, UserHash: user, Period: period,
		PerAsset: map[string]int64{}, RuleRef: "rp-nta-digital-assets:vasp.ringfence"}
	for _, gl := range e.ledger {
		if gl.TenantID != tenant || (user != "" && gl.UserHash != user) {
			continue
		}
		if period != "" && !strings.HasPrefix(gl.BookedAt, period) {
			continue
		}
		res.PerAsset[gl.Asset] += gl.GainLoss
		if gl.GainLoss >= 0 {
			res.GrossGains += gl.GainLoss
		} else {
			res.GrossLosses += -gl.GainLoss
		}
	}
	res.NetRingFenced = res.GrossGains - res.GrossLosses
	if res.NetRingFenced > 0 {
		res.TaxableGain = res.NetRingFenced
	} else {
		res.CarryForward = -res.NetRingFenced
	}
	return res
}
