package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// MBS pre-clearance client (SPEC §3 T1/T2): submit signed invoice → IRN +
// cryptographic stamp. The real MBS rail is behind the MBSClient interface;
// SandboxMBS is the full simulator used in dev.

// ClearanceResult is returned by MBS pre-clearance.
type ClearanceResult struct {
	IRN       string       `json:"irn"`
	Stamp     *CryptoStamp `json:"crypto_stamp"`
	Status    string       `json:"status"` // cleared|rejected
	Reason    string       `json:"reason,omitempty"`
	ClearedAt string       `json:"cleared_at"`
}

// B2CReportReceipt acknowledges a real-time B2C report.
type B2CReportReceipt struct {
	ReportID   string `json:"report_id"`
	IRN        string `json:"irn"`
	AcceptedAt string `json:"accepted_at"`
	Status     string `json:"status"`
}

// MBSClient is the adapter interface for the MBS rail.
type MBSClient interface {
	Name() string
	Preclear(ctx context.Context, inv *CanonicalInvoice, ublXML []byte) (*ClearanceResult, error)
	ReportB2C(ctx context.Context, inv *CanonicalInvoice) (*B2CReportReceipt, error)
}

// SandboxMBS is the full in-process MBS simulator: deterministic ed25519
// stamping key, monotonic IRN sequence, realistic payload semantics.
type SandboxMBS struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	seq  atomic.Int64
}

// NewSandboxMBS builds the simulator (stable key so stamps verify across runs).
func NewSandboxMBS() *SandboxMBS {
	seed := sha256.Sum256([]byte("meridian-mbs-sandbox-ed25519-seed-v1"))
	priv := ed25519.NewKeyFromSeed(seed[:])
	return &SandboxMBS{priv: priv, pub: priv.Public().(ed25519.PublicKey)}
}

func (m *SandboxMBS) Name() string { return "mbs-sandbox" }

// sandboxServiceID is the simulator's stand-in NRS integrator id (8
// alphanumeric chars) used when the invoice carries no valid ServiceID.
const sandboxServiceID = "MBSSIM01"

// PublicKeyHex lets verifiers check stamps out-of-band.
func (m *SandboxMBS) PublicKeyHex() string { return hex.EncodeToString(m.pub) }

func (m *SandboxMBS) stampPayload(irn, invoiceHash, ts string) string {
	return "MBS-STAMP-V1|" + irn + "|" + invoiceHash + "|" + ts
}

// Preclear simulates the MBS pre-clearance round trip.
func (m *SandboxMBS) Preclear(ctx context.Context, inv *CanonicalInvoice, ublXML []byte) (*ClearanceResult, error) {
	if inv.CSIDSignature == "" {
		return &ClearanceResult{Status: "rejected", Reason: "invoice not CSID-signed"}, nil
	}
	// Canonical IRN format (audit fix): the sandbox must mint the same
	// <invNum>-<svcID8>-<YYYYMMDD> shape as the official builder
	// (nrs_irn.go) so sim-flow IRNs never leak a divergent format into
	// stored invoices or QR payloads.
	svcID := inv.ServiceID
	if !ValidServiceID(svcID) {
		svcID = sandboxServiceID
	}
	irn, err := BuildIRN(inv.InvoiceNumber, svcID, inv.IssueDate)
	if err != nil {
		return &ClearanceResult{Status: "rejected", Reason: "IRN build: " + err.Error()}, nil
	}
	m.seq.Add(1)
	ts := time.Now().UTC().Format(time.RFC3339)
	payload := m.stampPayload(irn, inv.Hash(), ts)
	sig := ed25519.Sign(m.priv, []byte(payload))
	return &ClearanceResult{
		IRN:    irn,
		Status: "cleared",
		Stamp: &CryptoStamp{
			Algorithm: "ed25519", KeyID: "mbs-sandbox-2026",
			IRN: irn, Payload: payload, Signature: hex.EncodeToString(sig), StampedAt: ts,
		},
		ClearedAt: ts,
	}, nil
}

// ReportB2C accepts a real-time B2C fiscalisation report.
func (m *SandboxMBS) ReportB2C(ctx context.Context, inv *CanonicalInvoice) (*B2CReportReceipt, error) {
	n := m.seq.Add(1)
	return &B2CReportReceipt{
		ReportID:   fmt.Sprintf("B2C-RPT-%08d", n),
		IRN:        inv.IRN,
		AcceptedAt: time.Now().UTC().Format(time.RFC3339),
		Status:     "accepted",
	}, nil
}

// HTTPMBS is the real-rail adapter skeleton: active when MBS_BASE_URL is set.
type HTTPMBS struct {
	BaseURL string
	Client  *http.Client
}

func (h *HTTPMBS) Name() string { return "mbs-http" }

func (h *HTTPMBS) http() *http.Client {
	if h.Client != nil {
		return h.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (h *HTTPMBS) Preclear(ctx context.Context, inv *CanonicalInvoice, ublXML []byte) (*ClearanceResult, error) {
	body, _ := json.Marshal(map[string]any{
		"invoice": inv, "ubl_xml": string(ublXML),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(h.BaseURL, "/")+"/v1/preclearance", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mbs preclearance status %d", resp.StatusCode)
	}
	var out ClearanceResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (h *HTTPMBS) ReportB2C(ctx context.Context, inv *CanonicalInvoice) (*B2CReportReceipt, error) {
	body, _ := json.Marshal(map[string]any{"invoice": inv})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(h.BaseURL, "/")+"/v1/b2c/reports", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out B2CReportReceipt
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// NewMBSClient selects the adapter: real HTTP rail when MBS_BASE_URL is set,
// otherwise the sandbox simulator — DEV ONLY (QA-22). Under PROFILE=prod a
// regulated e-invoice submission must never be "precleared" by the sandbox
// simulator, so prod refuses to boot without an explicit MBS_BASE_URL
// (hard-fatal, same contract as the NIMC adapter in inclusion-suite).
func NewMBSClient() MBSClient {
	// Feature I1: MBS_PROFILE=sandbox|live selects the rail explicitly. Unset
	// preserves the legacy MBS_BASE_URL prod gate below (unchanged behavior).
	switch selectMBSProfile() {
	case "":
		// legacy gate (QA-22), unchanged
	case "sandbox":
		if os.Getenv("PROFILE") == "prod" || os.Getenv("PROFILE") == "production" {
			log.Fatal("PROFILE=prod FATAL: MBS_PROFILE=sandbox is not permitted in prod (fail-closed)")
		}
		log.Printf("profile=dev component=mbs-adapter rail=sandbox (explicit MBS_PROFILE)")
		return NewSandboxMBS()
	case "live":
		return newLiveRailOrFatal()
	default:
		log.Fatalf("MBS FATAL: unknown MBS_PROFILE %q (want sandbox|live)", os.Getenv(mbsProfileEnv))
	}
	if base := os.Getenv("MBS_BASE_URL"); base != "" {
		return &HTTPMBS{BaseURL: base}
	}
	if os.Getenv("PROFILE") == "prod" || os.Getenv("PROFILE") == "production" {
		log.Fatal("PROFILE=prod FATAL: MBS_BASE_URL or MBS_PROFILE=live is required (refusing to start with the MBS sandbox simulator)")
	}
	log.Printf("profile=dev component=mbs-adapter (sandbox simulator)")
	return NewSandboxMBS()
}
