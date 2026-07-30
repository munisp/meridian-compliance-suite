// Package envelope implements the Meridian event envelope (SPEC §1.1) and a
// dev in-process event bus plus a durable file outbox (SPEC §1.1 producer
// pattern). EVENT_BUS=inproc is the default; kafka wiring lives behind the
// same Publisher interface.
package envelope

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Envelope is the canonical event envelope carried on every nrs.* topic.
type Envelope struct {
	ID              string          `json:"id"`
	Type            string          `json:"type"`
	Source          string          `json:"source"`
	Time            string          `json:"time"`
	TenantID        string          `json:"tenant_id"`
	TraceID         string          `json:"trace_id"`
	RulePackVersion string          `json:"rule_pack_version"`
	Data            json.RawMessage `json:"data"`
}

// ulid generates a 26-char Crockford-base32 ULID (time-ordered, monotonic-ish).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	ulidMu   sync.Mutex
	ulidLast int64
	ulidRand []byte
)

func ULID() string {
	ulidMu.Lock()
	defer ulidMu.Unlock()
	now := time.Now().UnixMilli()
	if now < ulidLast {
		now = ulidLast
	}
	ulidLast = now
	if len(ulidRand) != 10 {
		ulidRand = make([]byte, 10)
	}
	if _, err := rand.Read(ulidRand); err != nil {
		panic(err)
	}
	var b [26]byte
	ms := uint64(now)
	for i := 9; i >= 0; i-- {
		b[i] = crockford[ms&31]
		ms >>= 5
	}
	rv := binary.BigEndian.Uint16(ulidRand[0:2])
	b[10] = crockford[rv>>11&31]
	b[11] = crockford[rv>>6&31]
	b[12] = crockford[rv>>1&31]
	b[13] = crockford[(rv&1)<<4|uint16(ulidRand[2]>>4)]
	rv2 := binary.BigEndian.Uint64(append([]byte{0}, ulidRand[2:9]...))
	_ = rv2
	x := uint64(0)
	for _, c := range ulidRand[2:10] {
		x = x<<8 | uint64(c)
	}
	for i := 25; i >= 14; i-- {
		b[i] = crockford[x&31]
		x >>= 5
	}
	return string(b[:])
}

// New builds an envelope with a fresh ULID, RFC3339 timestamp and trace id.
func New(eventType, source, tenantID, rulePackVersion string, data any) (Envelope, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Envelope{}, fmt.Errorf("marshal data: %w", err)
	}
	return Envelope{
		ID:              ULID(),
		Type:            eventType,
		Source:          source,
		Time:            time.Now().UTC().Format(time.RFC3339),
		TenantID:        tenantID,
		TraceID:         newTraceID(),
		RulePackVersion: rulePackVersion,
		Data:            raw,
	}, nil
}

func newTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b[:])
}

// Publisher is the event-bus abstraction. Implementations: InprocBus (dev
// default) and Outbox (durable file relay).
type Publisher interface {
	Publish(topic string, env Envelope) error
}

// InprocBus is the single-binary dev bus (EVENT_BUS=inproc).
type InprocBus struct {
	mu       sync.Mutex
	messages map[string][]Envelope
}

func NewInprocBus() *InprocBus { return &InprocBus{messages: map[string][]Envelope{}} }

func (b *InprocBus) Publish(topic string, env Envelope) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.messages[topic] = append(b.messages[topic], env)
	return nil
}

// Messages returns a copy of published messages for a topic (tests/dev).
func (b *InprocBus) Messages(topic string) []Envelope {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Envelope, len(b.messages[topic]))
	copy(out, b.messages[topic])
	return out
}

// OutboxRow is one durable outbox record.
type OutboxRow struct {
	Seq       int64     `json:"seq"`
	Topic     string    `json:"topic"`
	Envelope  Envelope  `json:"envelope"`
	Status    string    `json:"status"` // pending|published|failed
	Attempts  int       `json:"attempts"`
	CreatedAt time.Time `json:"created_at"`
}

// Outbox is a durable JSONL-file outbox (dev stand-in for the Postgres
// outbox+relay of SPEC §1.1). Appends are fsynced.
type Outbox struct {
	path string
	mu   sync.Mutex
	seq  int64
}

func NewOutbox(path string) (*Outbox, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	o := &Outbox{path: path}
	rows, err := o.Rows()
	if err == nil {
		for _, r := range rows {
			if r.Seq > o.seq {
				o.seq = r.Seq
			}
		}
	}
	return o, nil
}

// Publish appends the envelope durably with status=pending.
func (o *Outbox) Publish(topic string, env Envelope) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seq++
	row := OutboxRow{Seq: o.seq, Topic: topic, Envelope: env, Status: "pending", CreatedAt: time.Now().UTC()}
	return o.appendLocked(row)
}

func (o *Outbox) appendLocked(row OutboxRow) error {
	f, err := os.OpenFile(o.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// Rows replays the JSONL log, folding updates by seq (last write wins).
func (o *Outbox) Rows() ([]OutboxRow, error) {
	data, err := os.ReadFile(o.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	byID := map[int64]OutboxRow{}
	var order []int64
	start := 0
	for i, c := range data {
		if c != '\n' {
			continue
		}
		line := data[start:i]
		start = i + 1
		if len(line) == 0 {
			continue
		}
		var r OutboxRow
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("outbox corrupt at byte %d: %w", start, err)
		}
		if _, seen := byID[r.Seq]; !seen {
			order = append(order, r.Seq)
		}
		byID[r.Seq] = r
	}
	out := make([]OutboxRow, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out, nil
}

// Mark updates the status/attempts of a row (append-folded).
func (o *Outbox) Mark(seq int64, status string, attempts int) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	rows, err := o.Rows()
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Seq == seq {
			r.Status = status
			r.Attempts = attempts
			return o.appendLocked(r)
		}
	}
	return fmt.Errorf("outbox seq %d not found", seq)
}

// Relay is a Publisher that forwards an outbox's pending rows to a bus.
type Relay struct {
	Box *Outbox
	Bus Publisher
}

func (r *Relay) Publish(topic string, env Envelope) error { return r.Bus.Publish(topic, env) }

// Drain publishes all pending rows to the bus and marks them published.
func (r *Relay) Drain() (int, error) {
	rows, err := r.Box.Rows()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, row := range rows {
		if row.Status != "pending" && row.Status != "failed" {
			continue
		}
		if err := r.Bus.Publish(row.Topic, row.Envelope); err != nil {
			_ = r.Box.Mark(row.Seq, "failed", row.Attempts+1)
			continue
		}
		_ = r.Box.Mark(row.Seq, "published", row.Attempts+1)
		n++
	}
	return n, nil
}
