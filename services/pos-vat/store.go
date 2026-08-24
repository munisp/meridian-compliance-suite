package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- Store: in-mem hot path + embedded durable append-log fallback ----------
// Honesty tag: SPEC calls for a SQLite fallback; because the build sandbox has
// no module-proxy access (stdlib-only), the durable fallback is an embedded
// append-log store with the same durability contract (fsync per commit,
// replay on boot). Swap for SQLite/Postgres via the same interface in prod.

type Store struct {
	mu       sync.RWMutex
	receipts map[string]*Receipt
	byTenant map[string][]string
	recon    []*ReconRecord
	dir      string
	logFile  *os.File
}

type ReconRecord struct {
	ID             string `json:"id"`
	Period         string `json:"period"`
	TenantID       string `json:"tenant_id"`
	Receipts       int    `json:"receipts"`
	VATKobo        int64  `json:"vat_kobo"`
	FederalKobo    int64  `json:"federal_kobo"`
	StateKobo      int64  `json:"state_kobo"`
	LGAKobo        int64  `json:"lga_kobo"`
	Supplement     int    `json:"supplement,omitempty"` // 0 = initial settlement
	LedgerTransfer string `json:"ledger_transfer_id"`
	LedgerMode     string `json:"ledger_mode"` // core|dev
	PostedAt       string `json:"posted_at"`
}

func NewStore(dir string) *Store {
	st := &Store{receipts: map[string]*Receipt{}, byTenant: map[string][]string{}, dir: dir}
	os.MkdirAll(filepath.Join(dir, "spool"), 0o755)
	st.replay()
	f, err := os.OpenFile(filepath.Join(dir, "receipts.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		st.logFile = f
	}
	return st
}

func (st *Store) replay() {
	f, err := os.Open(filepath.Join(st.dir, "receipts.log"))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r Receipt
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.ID != "" {
			rc := r
			st.receipts[r.ID] = &rc
			st.byTenant[r.TenantID] = append(st.byTenant[r.TenantID], r.ID)
		}
	}
}

func (st *Store) PutReceipt(r *Receipt) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, dup := st.receipts[r.ID]; dup {
		return nil // idempotent
	}
	if st.logFile != nil {
		b, _ := json.Marshal(r)
		if _, err := st.logFile.Write(append(b, '\n')); err != nil {
			return err
		}
		st.logFile.Sync()
	}
	st.receipts[r.ID] = r
	st.byTenant[r.TenantID] = append(st.byTenant[r.TenantID], r.ID)
	return nil
}

// MarkReceiptsSettled durably stamps each receipt with the settlement it
// was remitted under (B3 #2). The appended log line is the last write for
// the receipt id, so replay restores the settled marker.
func (st *Store) MarkReceiptsSettled(ids []string, tenant, period string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, id := range ids {
		r, ok := st.receipts[id]
		if !ok {
			continue
		}
		r.SettledIn = settledKey(tenant, period)
		r.Status = "settled"
		if st.logFile != nil {
			b, _ := json.Marshal(r)
			if _, err := st.logFile.Write(append(b, '\n')); err != nil {
				return err
			}
		}
	}
	if st.logFile != nil {
		return st.logFile.Sync()
	}
	return nil
}

func (st *Store) GetReceipt(id string) (*Receipt, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	r, ok := st.receipts[id]
	return r, ok
}

func (st *Store) ListReceipts(tenant, state string, limit int) []*Receipt {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := []*Receipt{}
	for _, r := range st.receipts {
		if tenant != "" && r.TenantID != tenant {
			continue
		}
		if state != "" && !strings.EqualFold(r.State, state) {
			continue
		}
		out = append(out, r)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func (st *Store) AddRecon(rec *ReconRecord) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.recon = append(st.recon, rec)
}

func (st *Store) Recons() []*ReconRecord {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]*ReconRecord, len(st.recon))
	copy(out, st.recon)
	return out
}

// ---------- settled_periods: settlement marker table (F5) ----------
// Checked BEFORE any ledger posting: a re-run of an already-settled
// (tenant, period) is a 200 no-op replay; a "pending" marker means a
// crashed saga that the handler resumes from the persisted transfer ids.

type SettledPeriod struct {
	TenantID         string `json:"tenant_id"`
	Period           string `json:"period"`
	FederalKobo      int64  `json:"federal_kobo"`
	StateKobo        int64  `json:"state_kobo"`
	LGAKobo          int64  `json:"lga_kobo"` // B3 #2: LGA 35% share leg
	FederalPendingID string `json:"federal_pending_id,omitempty"`
	StatePendingID   string `json:"state_pending_id,omitempty"`
	LGAPendingID     string `json:"lga_pending_id,omitempty"`
	// Supplements counts supplemental settlements remitting receipts that
	// arrived AFTER this period first settled (B3 #2 carry-over).
	Supplements int    `json:"supplements,omitempty"`
	Status      string `json:"status"` // pending|settled|failed
	ReconID     string `json:"recon_id,omitempty"`
	FailReason  string `json:"fail_reason,omitempty"`
	UpdatedAt   string `json:"updated_at"`
}

var settledPeriods = struct {
	mu sync.RWMutex
	m  map[string]*SettledPeriod
}{m: map[string]*SettledPeriod{}}

func settledKey(tenant, period string) string { return tenant + "|" + period }

func (st *Store) loadSettledPeriods() {
	f, err := os.Open(filepath.Join(st.dir, "settled_periods.log"))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var sp SettledPeriod
		if json.Unmarshal(sc.Bytes(), &sp) == nil {
			settledPeriods.m[settledKey(sp.TenantID, sp.Period)] = &sp
		}
	}
}

// GetSettledPeriod returns the settlement marker for (tenant, period), or nil.
func (st *Store) GetSettledPeriod(tenant, period string) *SettledPeriod {
	settledPeriods.mu.RLock()
	defer settledPeriods.mu.RUnlock()
	if sp, ok := settledPeriods.m[settledKey(tenant, period)]; ok {
		cp := *sp
		return &cp
	}
	return nil
}

// SaveSettledPeriod upserts the marker and appends it to the durable log.
func (st *Store) SaveSettledPeriod(sp *SettledPeriod) error {
	settledPeriods.mu.Lock()
	settledPeriods.m[settledKey(sp.TenantID, sp.Period)] = sp
	settledPeriods.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(st.dir, "settled_periods.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(sp)
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// ---------- Spool: store-and-forward for offline terminals ----------

type SpoolEntry struct {
	ID        string          `json:"id"`
	QueuedAt  string          `json:"queued_at"`
	Reason    string          `json:"reason"`
	Payload   json.RawMessage `json:"payload"`
	DrainedAt string          `json:"drained_at,omitempty"`
}

func (st *Store) Spool(e SpoolEntry) error {
	b, _ := json.Marshal(e)
	return os.WriteFile(filepath.Join(st.dir, "spool", e.ID+".json"), b, 0o644)
}

func (st *Store) SpoolList() []SpoolEntry {
	out := []SpoolEntry{}
	entries, _ := os.ReadDir(filepath.Join(st.dir, "spool"))
	for _, en := range entries {
		b, err := os.ReadFile(filepath.Join(st.dir, "spool", en.Name()))
		if err != nil {
			continue
		}
		var e SpoolEntry
		if json.Unmarshal(b, &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func (st *Store) SpoolRemove(id string) {
	os.Remove(filepath.Join(st.dir, "spool", id+".json"))
}

// ---------- Cache: Redis (optional) with in-mem fallback ----------

type Cache interface {
	SetNX(key, val string, ttl time.Duration) (bool, error)
	Get(key string) (string, error)
}

type MemCache struct {
	mu   sync.Mutex
	data map[string]memEntry
}

type memEntry struct {
	val string
	exp time.Time
}

func NewMemCache() *MemCache { return &MemCache{data: map[string]memEntry{}} }

func (m *MemCache) SetNX(key, val string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.data[key]; ok && time.Now().Before(e.exp) {
		return false, nil
	}
	m.data[key] = memEntry{val, time.Now().Add(ttl)}
	return true, nil
}

func (m *MemCache) Get(key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.data[key]; ok && time.Now().Before(e.exp) {
		return e.val, nil
	}
	return "", errors.New("miss")
}

// RedisCache is a minimal RESP client (no external deps). Optional hot path.
type RedisCache struct{ Addr string }

func NewRedisCache(url string) *RedisCache {
	addr := strings.TrimPrefix(url, "redis://")
	if !strings.Contains(addr, ":") {
		addr += ":6379"
	}
	return &RedisCache{Addr: addr}
}

func (r *RedisCache) cmd(args ...string) (string, error) {
	conn, err := net.DialTimeout("tcp", r.Addr, 800*time.Millisecond)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(1200 * time.Millisecond))
	var sb strings.Builder
	fmt.Fprintf(&sb, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&sb, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := conn.Write([]byte(sb.String())); err != nil {
		return "", err
	}
	rd := bufio.NewReader(conn)
	line, err := rd.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	switch {
	case strings.HasPrefix(line, "+"):
		return line[1:], nil
	case strings.HasPrefix(line, "-"):
		return "", errors.New(line[1:])
	case strings.HasPrefix(line, "$"):
		n, _ := strconv.Atoi(line[1:])
		if n < 0 {
			return "", errors.New("nil")
		}
		buf := make([]byte, n+2)
		if _, err := readFull(rd, buf); err != nil {
			return "", err
		}
		return string(buf[:n]), nil
	case strings.HasPrefix(line, ":"):
		return line[1:], nil
	}
	return line, nil
}

func readFull(rd *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := rd.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (r *RedisCache) Ping() error {
	_, err := r.cmd("PING")
	return err
}

func (r *RedisCache) SetNX(key, val string, ttl time.Duration) (bool, error) {
	res, err := r.cmd("SET", key, val, "PX", strconv.FormatInt(ttl.Milliseconds(), 10), "NX")
	if err != nil {
		if err.Error() == "nil" {
			return false, nil
		}
		return false, err
	}
	return res == "OK", nil
}

func (r *RedisCache) Get(key string) (string, error) { return r.cmd("GET", key) }

// ---------- In-process event bus + file outbox (SPEC §1.1 dev fallback) ----------

type InprocBus struct {
	mu     sync.Mutex
	events []Envelope
	outbox *os.File
}

func NewInprocBus() *InprocBus { return &InprocBus{} }

func (b *InprocBus) Publish(dir, topic, tenant, packVersion string, data any) (*Envelope, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	env := Envelope{
		ID: ULID(), Type: topic, Source: "pos-vat", Time: nowRFC3339(),
		TenantID: tenant, TraceID: ULID(), RulePackVersion: packVersion, Data: raw,
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.outbox == nil {
		f, err := os.OpenFile(filepath.Join(dir, "outbox.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			b.outbox = f
		}
	}
	if b.outbox != nil {
		eb, _ := json.Marshal(env)
		b.outbox.Write(append(eb, '\n'))
		b.outbox.Sync()
	}
	b.events = append(b.events, env)
	if len(b.events) > 500 {
		b.events = b.events[len(b.events)-500:]
	}
	return &env, nil
}

func (b *InprocBus) Recent(limit int) []Envelope {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Envelope, 0, len(b.events))
	for i := len(b.events) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, b.events[i])
	}
	return out
}
