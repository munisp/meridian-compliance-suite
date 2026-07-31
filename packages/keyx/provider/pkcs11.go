package provider

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// PKCS#11 HSM provider — CGO-free by design.
//
// [REAL] INTERFACE: builds stay CGO-free, so the PKCS#11 module (vendor
// .so) is never linked into Meridian binaries. Real HSM wiring is via a
// documented plugin binary/exec protocol: KEY_PKCS11_PLUGIN points at a
// small helper binary (built with CGO against the vendor PKCS#11 library,
// e.g. using miekg/pkcs11 in a separate module) that the provider execs and
// speaks to over stdin/stdout as newline-delimited JSON:
//
//	request:  {"op":"sign"|"pubkey"|"rotate"|"ping","key_id":"csid",
//	           "payload_b64":"...","mechanism":"ed25519|hmac-sha256"}
//	response: {"ok":true,"signature_b64":"...","public_key_b64":"..."}
//	          or {"ok":false,"error":"..."}
//
// One request per line, one response per line, process is long-lived. A
// reference plugin lives out-of-tree (it needs the vendor SDK + CGO); the
// protocol above is the stability contract.
//
// [SIM] SOFT-TOKEN: NewSoftToken returns an in-process SignerProvider that
// implements the exact same semantics with software keys, for tests and dev
// environments without an HSM. It is honestly tagged simulated — production
// MUST use the exec plugin (fail-closed when KEY_PKCS11_PLUGIN is unset or
// the plugin cannot be started).

// PKCS11Plugin is the [REAL] exec-protocol client for an out-of-process
// PKCS#11 plugin binary.
type PKCS11Plugin struct {
	pluginPath string
	mu         sync.Mutex
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	reader     *bufio.Reader
}

// NewPKCS11Plugin validates and lazily starts the plugin binary. The binary
// must exist and be executable; the process is spawned on first use.
func NewPKCS11Plugin(pluginPath string) (*PKCS11Plugin, error) {
	st, err := os.Stat(pluginPath)
	if err != nil {
		return nil, fmt.Errorf("pkcs11 plugin: %v", err)
	}
	if st.IsDir() || st.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("pkcs11 plugin %q is not an executable file", pluginPath)
	}
	return &PKCS11Plugin{pluginPath: pluginPath}, nil
}

// Mode implements SignerProvider.
func (p *PKCS11Plugin) Mode() string { return "pkcs11" }

type pluginRequest struct {
	Op         string `json:"op"`
	KeyID      string `json:"key_id"`
	PayloadB64 string `json:"payload_b64,omitempty"`
	Mechanism  string `json:"mechanism,omitempty"`
}

type pluginResponse struct {
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
	SignatureB64 string `json:"signature_b64,omitempty"`
	PublicKeyB64 string `json:"public_key_b64,omitempty"`
}

func (p *PKCS11Plugin) callLocked(req pluginRequest) (*pluginResponse, error) {
	if p.cmd == nil {
		cmd := exec.Command(p.pluginPath)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("%w: start pkcs11 plugin: %v", ErrUnavailable, err)
		}
		p.cmd, p.stdin, p.reader = cmd, stdin, bufio.NewReader(stdout)
	}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := p.stdin.Write(append(line, '\n')); err != nil {
		p.killLocked()
		return nil, fmt.Errorf("%w: pkcs11 plugin write: %v", ErrUnavailable, err)
	}
	respLine, err := p.reader.ReadBytes('\n')
	if err != nil {
		p.killLocked()
		return nil, fmt.Errorf("%w: pkcs11 plugin read: %v", ErrUnavailable, err)
	}
	var resp pluginResponse
	if err := json.Unmarshal(respLine, &resp); err != nil {
		return nil, fmt.Errorf("pkcs11 plugin: malformed response: %v", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("pkcs11 plugin: %s", resp.Error)
	}
	return &resp, nil
}

func (p *PKCS11Plugin) killLocked() {
	if p.cmd != nil {
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
		p.cmd, p.stdin, p.reader = nil, nil, nil
	}
}

func mechanism(keyID string) string {
	if IsHMACKeyID(keyID) {
		return "hmac-sha256"
	}
	return "ed25519"
}

// Sign implements SignerProvider via the plugin exec protocol.
func (p *PKCS11Plugin) Sign(_ context.Context, keyID string, payload []byte) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	resp, err := p.callLocked(pluginRequest{
		Op: "sign", KeyID: keyID, Mechanism: mechanism(keyID),
		PayloadB64: base64.StdEncoding.EncodeToString(payload),
	})
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(resp.SignatureB64)
}

// PublicKey implements SignerProvider via the plugin exec protocol.
func (p *PKCS11Plugin) PublicKey(_ context.Context, keyID string) ([]byte, error) {
	if IsHMACKeyID(keyID) {
		return nil, ErrSymmetricKey
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	resp, err := p.callLocked(pluginRequest{Op: "pubkey", KeyID: keyID, Mechanism: mechanism(keyID)})
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(resp.PublicKeyB64)
}

// Rotate implements SignerProvider via the plugin exec protocol.
func (p *PKCS11Plugin) Rotate(_ context.Context, keyID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.callLocked(pluginRequest{Op: "rotate", KeyID: keyID, Mechanism: mechanism(keyID)})
	return err
}

// Close terminates the plugin process.
func (p *PKCS11Plugin) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killLocked()
}

// --- [SIM] soft-token -------------------------------------------------------

// SoftToken is a [SIM] in-process PKCS#11-style soft token implementing the
// SignerProvider semantics with software ed25519/HMAC keys. TESTS AND DEV
// ONLY — production HSM signing goes through PKCS11Plugin (exec protocol).
type SoftToken struct {
	mu   sync.Mutex
	keys map[string]ed25519.PrivateKey
	macs map[string][]byte
}

// NewSoftToken returns an empty [SIM] soft-token provider.
func NewSoftToken() *SoftToken {
	return &SoftToken{keys: map[string]ed25519.PrivateKey{}, macs: map[string][]byte{}}
}

// Mode implements SignerProvider.
func (t *SoftToken) Mode() string { return "pkcs11" }

// Sign implements SignerProvider ([SIM] soft-token).
func (t *SoftToken) Sign(_ context.Context, keyID string, payload []byte) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if IsHMACKeyID(keyID) {
		key, ok := t.macs[keyID]
		if !ok {
			key = make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return nil, err
			}
			t.macs[keyID] = key
		}
		mac := hmac.New(sha256.New, key)
		mac.Write(payload)
		return mac.Sum(nil), nil
	}
	priv, ok := t.keys[keyID]
	if !ok {
		var err error
		_, priv, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		t.keys[keyID] = priv
	}
	return ed25519.Sign(priv, payload), nil
}

// PublicKey implements SignerProvider ([SIM] soft-token).
func (t *SoftToken) PublicKey(_ context.Context, keyID string) ([]byte, error) {
	if IsHMACKeyID(keyID) {
		return nil, ErrSymmetricKey
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	priv, ok := t.keys[keyID]
	if !ok {
		var err error
		_, priv, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		t.keys[keyID] = priv
	}
	pub := priv.Public().(ed25519.PublicKey)
	out := make([]byte, len(pub))
	copy(out, pub)
	return out, nil
}

// Rotate implements SignerProvider ([SIM] soft-token).
func (t *SoftToken) Rotate(_ context.Context, keyID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if IsHMACKeyID(keyID) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		t.macs[keyID] = key
		return nil
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	t.keys[keyID] = priv
	return nil
}
