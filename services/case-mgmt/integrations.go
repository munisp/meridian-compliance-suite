package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ---------- WORM evidence: core audit-evidence API + local fallback ----------

type EvidenceReceipt struct {
	ID       string `json:"id"`
	SHA256   string `json:"sha256"`
	WORMURI  string `json:"worm_uri"`
	Source   string `json:"source"` // core|local
	StoredAt string `json:"stored_at"`
}

type WORMClient interface {
	Store(kind string, payload []byte, meta map[string]string) (*EvidenceReceipt, error)
}

type HTTPWORM struct{ Base string }

func (h *HTTPWORM) Store(kind string, payload []byte, meta map[string]string) (*EvidenceReceipt, error) {
	sum := sha256.Sum256(payload)
	body, _ := json.Marshal(map[string]any{
		"kind": kind, "sha256": hex.EncodeToString(sum[:]),
		"payload_b64": payload, "meta": meta,
	})
	cli := &http.Client{Timeout: 3 * time.Second}
	resp, err := cli.Post(h.Base+"/v1/evidence", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("worm %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		ID      string `json:"id"`
		WORMURI string `json:"worm_uri"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return &EvidenceReceipt{
		ID: orDef2(out.ID, "ev-"+ULID()), SHA256: hex.EncodeToString(sum[:]),
		WORMURI: orDef2(out.WORMURI, "worm://core/"+out.ID), Source: "core", StoredAt: nowRFC3339(),
	}, nil
}

// LocalWORM is the dev fallback: content-addressed immutable files on disk.
type LocalWORM struct{ Dir string }

func (l *LocalWORM) Store(kind string, payload []byte, meta map[string]string) (*EvidenceReceipt, error) {
	sum := sha256.Sum256(payload)
	hexsum := hex.EncodeToString(sum[:])
	if err := os.MkdirAll(filepath.Join(l.Dir, "worm"), 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(l.Dir, "worm", hexsum+".bin")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, payload, 0o444); err != nil { // read-only = immutable
			return nil, err
		}
	}
	manifest := map[string]any{"sha256": hexsum, "kind": kind, "meta": meta, "stored_at": nowRFC3339()}
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	os.WriteFile(filepath.Join(l.Dir, "worm", hexsum+".json"), mb, 0o444)
	return &EvidenceReceipt{
		ID: "ev-" + hexsum[:16], SHA256: hexsum,
		WORMURI: "worm://local/" + hexsum, Source: "local", StoredAt: nowRFC3339(),
	}, nil
}

func orDef2(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ---------- notifications: core notification svc API + log fallback ----------

type Notifier interface {
	Send(channel, to, subject, body string) (string, error)
}

type HTTPNotifier struct{ Base string }

func (n *HTTPNotifier) Send(channel, to, subject, body string) (string, error) {
	payload, _ := json.Marshal(map[string]string{
		"channel": channel, "to": to, "subject": subject, "body": body,
	})
	cli := &http.Client{Timeout: 3 * time.Second}
	resp, err := cli.Post(n.Base+"/v1/send", "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("notification %d", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return orDef2(out.ID, "ntf-"+ULID()), nil
}

// LogNotifier is the dev fallback: structured log lines.
type LogNotifier struct{}

func (LogNotifier) Send(channel, to, subject, body string) (string, error) {
	id := "ntf-" + ULID()
	logm("notify", fmt.Sprintf("%s -> %s | %s | %s (id=%s)", channel, to, subject, truncate(body, 80), id))
	return id, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
