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
	"time"
)

// ---------- domain model ----------

type Matter struct {
	ID          string   `json:"id"`
	TenantID    string   `json:"tenant_id"`
	Ref         string   `json:"ref"` // e.g. MTR-2026-0001
	Title       string   `json:"title"`
	ClientID    string   `json:"client_id"`
	ClientName  string   `json:"client_name"`
	PracticeArea string  `json:"practice_area"` // tax-appeal|audit-defence|advisory|ombud-referral
	Status      string   `json:"status"`        // intake|active|on-hold|closed
	Counsel     []string `json:"counsel"`       // user ids
	OpenedAt    string   `json:"opened_at"`
	ClosedAt    string   `json:"closed_at,omitempty"`
	Notes       string   `json:"notes,omitempty"`
}

type Document struct {
	ID         string `json:"id"`
	MatterID   string `json:"matter_id"`
	Name       string `json:"name"`
	MimeType   string `json:"mime_type"`
	SizeBytes  int64  `json:"size_bytes"`
	SHA256     string `json:"sha256"`
	Privileged bool   `json:"privileged"` // doc#privileged relation
	UploadedBy string `json:"uploaded_by"`
	UploadedAt string `json:"uploaded_at"`
	Content    []byte `json:"-"` // stored on disk, never marshalled in lists
}

type Deadline struct {
	ID        string `json:"id"`
	MatterID  string `json:"matter_id"`
	Title     string `json:"title"`
	DueAt     string `json:"due_at"` // RFC3339
	Severity  string `json:"severity"` // info|warning|critical
	Status    string `json:"status"`   // open|met|missed|escalated
	Escalated bool   `json:"escalated"`
	CreatedAt string `json:"created_at"`
}

// ---------- store: in-mem indexes + embedded append-log durability ----------

type Store struct {
	mu        sync.Mutex
	dir       string
	matters   map[string]*Matter
	docs      map[string]*Document
	deadlines map[string]*Deadline
	seq       int
	logFile   *os.File
}

func NewStore(dir string) *Store {
	st := &Store{dir: dir, matters: map[string]*Matter{}, docs: map[string]*Document{}, deadlines: map[string]*Deadline{}}
	os.MkdirAll(filepath.Join(dir, "blobs"), 0o755)
	st.replay()
	f, err := os.OpenFile(filepath.Join(dir, "case.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		st.logFile = f
	}
	return st
}

type logRec struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

func (st *Store) appendLog(kind string, v any) {
	if st.logFile == nil {
		return
	}
	b, _ := json.Marshal(logRec{Kind: kind, Data: json.RawMessage(mustJSON(v))})
	st.logFile.Write(append(b, '\n'))
	st.logFile.Sync()
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func (st *Store) replay() {
	f, err := os.Open(filepath.Join(st.dir, "case.log"))
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
		case "matter":
			var m Matter
			if json.Unmarshal(rec.Data, &m) == nil {
				cp := m
				st.matters[m.ID] = &cp
			}
		case "doc":
			var d Document
			if json.Unmarshal(rec.Data, &d) == nil {
				cp := d
				st.docs[d.ID] = &cp
			}
		case "deadline":
			var dl Deadline
			if json.Unmarshal(rec.Data, &dl) == nil {
				cp := dl
				st.deadlines[dl.ID] = &cp
			}
		}
	}
}

func (st *Store) nextRef() string {
	st.seq++
	return fmt.Sprintf("MTR-%d-%04d", time.Now().Year(), st.seq)
}

func (st *Store) PutMatter(m *Matter) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.matters[m.ID] = m
	st.appendLog("matter", m)
}

func (st *Store) GetMatter(id string) (*Matter, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	m, ok := st.matters[id]
	return m, ok
}

func (st *Store) ListMatters(tenant, status, clientID string) []*Matter {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := []*Matter{}
	for _, m := range st.matters {
		if tenant != "" && m.TenantID != tenant {
			continue
		}
		if status != "" && m.Status != status {
			continue
		}
		if clientID != "" && m.ClientID != clientID {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenedAt > out[j].OpenedAt })
	return out
}

func (st *Store) PutDocument(d *Document, content []byte) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := os.WriteFile(filepath.Join(st.dir, "blobs", d.ID), content, 0o644); err != nil {
		return err
	}
	st.docs[d.ID] = d
	st.appendLog("doc", d)
	return nil
}

func (st *Store) GetDocument(id string) (*Document, []byte, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	d, ok := st.docs[id]
	if !ok {
		return nil, nil, false
	}
	b, err := os.ReadFile(filepath.Join(st.dir, "blobs", id))
	if err != nil {
		return d, nil, true
	}
	return d, b, true
}

func (st *Store) ListDocuments(matterID string, includePrivileged bool) []*Document {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := []*Document{}
	for _, d := range st.docs {
		if d.MatterID != matterID {
			continue
		}
		if d.Privileged && !includePrivileged {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UploadedAt > out[j].UploadedAt })
	return out
}

func (st *Store) PutDeadline(dl *Deadline) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.deadlines[dl.ID] = dl
	st.appendLog("deadline", dl)
}

func (st *Store) ListDeadlines(matterID, status string, dueBefore string) []*Deadline {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := []*Deadline{}
	for _, dl := range st.deadlines {
		if matterID != "" && dl.MatterID != matterID {
			continue
		}
		if status != "" && dl.Status != status {
			continue
		}
		if dueBefore != "" && dl.DueAt > dueBefore {
			continue
		}
		out = append(out, dl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DueAt < out[j].DueAt })
	return out
}

func (st *Store) GetDeadline(id string) (*Deadline, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	dl, ok := st.deadlines[id]
	return dl, ok
}

// ---------- Permify-style dev file-backed relation checker ----------
// Relations: matter#client@user:<id>, matter#counsel@user:<id>, doc#privileged@role:counsel
// Model mirrors core packages/permify-models (pinned core contracts v1).

type RelationTuple struct {
	Entity   string `json:"entity"`   // matter:ID | doc:ID
	Relation string `json:"relation"` // client|counsel|privileged
	Subject  string `json:"subject"`  // user:ID | role:counsel
}

type RelationChecker struct {
	mu     sync.Mutex
	file   string
	tuples []RelationTuple
}

func NewRelationChecker(file string) *RelationChecker {
	rc := &RelationChecker{file: file}
	rc.load()
	return rc
}

func (rc *RelationChecker) load() {
	rc.tuples = nil
	if b, err := os.ReadFile(rc.file); err == nil {
		json.Unmarshal(b, &rc.tuples)
	}
}

func (rc *RelationChecker) persist() {
	b, _ := json.MarshalIndent(rc.tuples, "", "  ")
	os.WriteFile(rc.file, b, 0o644)
}

func (rc *RelationChecker) Grant(t RelationTuple) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for _, e := range rc.tuples {
		if e == t {
			return
		}
	}
	rc.tuples = append(rc.tuples, t)
	rc.persist()
}

func (rc *RelationChecker) Revoke(entity, relation, subject string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	out := rc.tuples[:0]
	for _, e := range rc.tuples {
		if !(e.Entity == entity && e.Relation == relation && e.Subject == subject) {
			out = append(out, e)
		}
	}
	rc.tuples = out
	rc.persist()
}

// Check answers "can <subject> do <permission> on <entity>?" with the dev
// permify-style semantics:
//   - matter read: subject is matter#client or matter#counsel
//   - matter write: subject is matter#counsel
//   - doc read: doc not privileged, or subject is counsel on parent matter
//     (doc#privileged marks counsel-only visibility)
func (rc *RelationChecker) Check(entity, permission, subject string, store *Store) bool {
	rc.mu.Lock()
	tuples := make([]RelationTuple, len(rc.tuples))
	copy(tuples, rc.tuples)
	rc.mu.Unlock()
	has := func(ent, rel, sub string) bool {
		for _, e := range tuples {
			if e.Entity == ent && e.Relation == rel && e.Subject == sub {
				return true
			}
		}
		return false
	}
	parts := strings.SplitN(entity, ":", 2)
	kind := parts[0]
	switch {
	case kind == "matter" && permission == "read":
		return has(entity, "client", subject) || has(entity, "counsel", subject)
	case kind == "matter" && permission == "write":
		return has(entity, "counsel", subject)
	case kind == "doc" && permission == "read":
		docID := parts[1]
		d, _, ok := store.GetDocument(docID)
		if !ok {
			return false
		}
		if !d.Privileged {
			// any matter participant may read non-privileged docs
			return rc.Check("matter:"+d.MatterID, "read", subject, store)
		}
		// privileged: counsel of the parent matter only (doc#privileged)
		return has("doc:"+docID, "privileged", subject) || has("matter:"+d.MatterID, "counsel", subject)
	}
	return false
}

func (rc *RelationChecker) Tuples() []RelationTuple {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	out := make([]RelationTuple, len(rc.tuples))
	copy(out, rc.tuples)
	return out
}
