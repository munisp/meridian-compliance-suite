// Command case-mgmt is the Meridian T13-practitioner case management service:
// matters CRUD, privileged documents, deadlines with escalation, client
// portal API, Permify-style relation checks (dev file-backed checker),
// evidence-pack builder via core WORM API (local fallback), and the wf-case-*
// workflow set with deadline watch + notifications. Dev-standalone (SPEC §1.3).
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---------- shared helpers (local copies; pins core contracts v1) ----------

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func logm(level, msg string) { fmt.Printf("[%s] %s %s\n", nowRFC3339(), level, msg) }

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	ulidMu   sync.Mutex
	ulidLast int64
)

func ULID() string {
	ulidMu.Lock()
	defer ulidMu.Unlock()
	now := time.Now().UnixMilli()
	if now < ulidLast {
		now = ulidLast
	}
	ulidLast = now
	var b [16]byte
	binary.BigEndian.PutUint64(b[0:8], uint64(now))
	rand.Read(b[8:])
	bits := make([]byte, 0, 130)
	for _, by := range b {
		for i := 7; i >= 0; i-- {
			bits = append(bits, (by>>uint(i))&1)
		}
	}
	var out [26]byte
	for i := 0; i < 26; i++ {
		var v byte
		for j := 0; j < 5; j++ {
			idx := i*5 + j
			if idx < len(bits) {
				v = v<<1 | bits[idx]
			} else {
				v <<= 1
			}
		}
		out[i] = crockford[v]
	}
	return string(out[:])
}

func TINHash(v, key string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(v))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeProblem(w http.ResponseWriter, code int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(Problem{
		Type: fmt.Sprintf("https://meridian.ng/problems/%d", code),
		Title: title, Status: code, Detail: detail,
	})
}

// ---------- dev HS256 JWT ----------

type Claims struct {
	Sub      string   `json:"sub"`
	Roles    []string `json:"roles"`
	TenantID string   `json:"tenant_id"`
	Exp      int64    `json:"exp"`
}

func verifyHS256(token, secret string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed token")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(mac.Sum(nil), sig) {
		return nil, fmt.Errorf("bad signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, err
	}
	if c.Exp != 0 && time.Now().Unix() > c.Exp {
		return nil, fmt.Errorf("token expired")
	}
	return &c, nil
}
