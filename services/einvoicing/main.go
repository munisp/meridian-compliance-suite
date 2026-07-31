// einvoicing — Meridian T1/T2: canonical invoices, UBL 2.1/Peppol BIS
// mapping, rp-ubl-bis + rp-mbs-business-rules validation, CSID signing, MBS
// pre-clearance (sandbox simulator), B2C real-time reporting, durable replay
// queue, multi-APP routing, ERP adapters, wf-mbs-preclearance.
package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/munisp/meridian-compliance-suite/packages/prodx"
	"github.com/munisp/meridian-compliance-suite/packages/shared/devjwt"
	"github.com/munisp/meridian-compliance-suite/packages/shared/envelope"
)

// kafkaBus adapts the franz-go producer (KAFKA_BROKERS, H1/H3) to the
// envelope.Publisher interface used by the outbox relay.
type kafkaBus struct{ prod *prodx.Producer }

func (b *kafkaBus) Publish(topic string, env envelope.Envelope) error {
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return b.prod.Publish(ctx, topic, []byte(env.ID), raw)
}

const serviceName = "einvoicing"
const serviceVersion = "1.0.0"

// Server wires the service's dependencies.
type Server struct {
	store     *InvoiceStore
	outbox    *envelope.Outbox
	signer    *CSIDSigner
	validator *Validator
	router    *APPRouter
	runner    *InprocRunner
}

func dataDir() string {
	if d := os.Getenv("DATA_DIR"); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "meridian-einvoicing")
}

func newInvoiceEvent(eventType string, inv *CanonicalInvoice) (envelope.Envelope, error) {
	return envelope.New(eventType, serviceName, inv.TenantID, "rp-mbs-business-rules@1.0.0", map[string]any{
		"invoice_id": inv.ID, "invoice_number": inv.InvoiceNumber,
		"irn": inv.IRN, "status": inv.Status, "payable_kobo": inv.PayableKobo,
		"supplier_tin_hash": inv.Hash()[:16],
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	dir := dataDir()
	store, err := NewInvoiceStore(filepath.Join(dir, "invoices.jsonl"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	// H1: DATABASE_URL selects the Postgres durable store (dev: JSONL file).
	ctx := context.Background()
	if pool, err := prodx.PGFromEnv(ctx); err != nil {
		log.Printf("postgres unavailable, staying on dev store: %v", err)
	} else if pool != nil {
		docs, err := prodx.NewDocStore(ctx, pool, "einvoicing")
		if err != nil {
			log.Printf("postgres docstore: %v (staying on dev store)", err)
		} else if err := store.SetPG(ctx, docs); err != nil {
			log.Printf("postgres load: %v (staying on dev store)", err)
		}
	}
	outbox, err := envelope.NewOutbox(filepath.Join(dir, "outbox.jsonl"))
	if err != nil {
		log.Fatalf("outbox: %v", err)
	}
	// H1/H3: KAFKA_BROKERS selects the real (franz-go) producer behind the
	// outbox relay; dev keeps the in-process bus.
	var bus envelope.Publisher = envelope.NewInprocBus()
	if prod, err := prodx.ProducerFromEnv(); err != nil {
		log.Printf("kafka unavailable, staying on inproc bus: %v", err)
	} else if prod != nil {
		bus = &kafkaBus{prod: prod}
		defer prod.Close()
	}
	relay := &envelope.Relay{Box: outbox, Bus: bus}
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for range tick.C {
			if _, err := relay.Drain(); err != nil {
				log.Printf("outbox relay: %v", err)
			}
		}
	}()
	signer, err := LoadCSID(dir)
	if err != nil {
		log.Fatalf("csid: %v", err)
	}
	srv := &Server{
		store: store, outbox: outbox, signer: signer,
		validator: NewValidator(),
		router:    NewAPPRouter(NewMBSClient()),
		runner:    NewInprocRunner(),
	}
	registerWorkflows(srv.runner)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "service": serviceName, "version": serviceVersion})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"status": "ready", "packs": srv.validator.PackVersions()})
	})
	mux.HandleFunc("POST /v1/invoices", srv.handleCreateInvoice)
	mux.HandleFunc("GET /v1/invoices/{id}", srv.handleGetInvoice)
	mux.HandleFunc("POST /v1/invoices/{id}/preclear", srv.handlePreclear)
	mux.HandleFunc("GET /v1/invoices/{id}/qr", srv.handleGetQR)
	mux.HandleFunc("POST /v1/b2c/report", srv.handleB2CReport)
	mux.HandleFunc("GET /v1/replay", srv.handleReplayList)
	mux.HandleFunc("POST /v1/replay/{seq}", srv.handleReplayOne)
	mux.HandleFunc("GET /v1/workflows", srv.handleWorkflows)
	mux.HandleFunc("GET /v1/apps", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, srv.router.List())
	})
	mux.HandleFunc("GET /v1/csid/public-key", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"key_id": srv.signer.KeyID(), "public_key_hex": srv.signer.PublicKeyHex()})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8110"
	}
	log.Printf("%s %s listening on :%s (data dir %s)", serviceName, serviceVersion, port, dir)
	log.Fatal(http.ListenAndServe(":"+port, devjwt.Middleware(mux)))
}

// handleCreateInvoice ingests via REST/CSV/SAP-OData adapters with
// idempotency keys (Idempotency-Key header) and offline-queue semantics.
func (s *Server) handleCreateInvoice(w http.ResponseWriter, r *http.Request) {
	adapterName := r.URL.Query().Get("adapter")
	// bounded read: reject payloads > 4 MiB (audit fix: unbounded body -> memory DoS)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		devjwt.Problem(w, 413, "payload too large", err.Error())
		return
	}
	adapter, err := AdapterFor(adapterName, r.Header.Get("Content-Type"), body)
	if err != nil {
		devjwt.Problem(w, 400, "bad adapter", err.Error())
		return
	}
	invs, err := adapter.Parse(body)
	if err != nil {
		devjwt.Problem(w, 422, "parse failed", err.Error())
		return
	}
	claims, _ := devjwt.FromContext(r)
	created := make([]*CanonicalInvoice, 0, len(invs))
	for _, inv := range invs {
		inv.Normalise()
		inv.TenantID = claims.TenantID
		inv.IdempotencyKey = r.Header.Get("Idempotency-Key")
		if inv.SourceAdapter == "" {
			inv.SourceAdapter = adapter.Name()
		}
		violations, fatal, err := s.validator.Validate(inv, s.store.IsDuplicate(inv))
		if err != nil {
			devjwt.Problem(w, 500, "validation error", err.Error())
			return
		}
		inv.Validation = violations
		inv.Status = "validated"
		if fatal {
			inv.Status = "failed"
		}
		priorID, err := s.store.Save(inv)
		if priorID != "" {
			prior, _ := s.store.Get(priorID)
			writeJSON(w, 200, map[string]any{
				"idempotent_replay": true, "invoice": prior,
			})
			return
		}
		if err != nil {
			devjwt.Problem(w, 500, "persist failed", err.Error())
			return
		}
		env, err := newInvoiceEvent("nrs.mbs.invoice.received.v1", inv)
		if err == nil {
			_ = s.outbox.Publish("nrs.mbs.invoice.received.v1", env)
		}
		created = append(created, inv)
	}
	writeJSON(w, 201, map[string]any{"invoices": created, "count": len(created)})
}

func (s *Server) handleGetInvoice(w http.ResponseWriter, r *http.Request) {
	inv, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		devjwt.Problem(w, 404, "not found", "invoice "+r.PathValue("id"))
		return
	}
	if claims, cerr := devjwt.FromContext(r); cerr == nil && claims.TenantID != "" &&
		inv.TenantID != "" && inv.TenantID != claims.TenantID {
		// tenant isolation: never reveal existence across tenants (audit fix)
		devjwt.Problem(w, 404, "not found", "invoice "+r.PathValue("id"))
		return
	}
	if r.URL.Query().Get("format") == "ubl" {
		xmlBytes, err := GenerateUBL(inv)
		if err != nil {
			devjwt.Problem(w, 500, "ubl error", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write(xmlBytes)
		return
	}
	writeJSON(w, 200, inv)
}

// handleGetQR returns the NRS verification QR (payload + HMAC signature + SVG)
// for a cleared invoice (MBS hard requirement: QR on the issued invoice).
func (s *Server) handleGetQR(w http.ResponseWriter, r *http.Request) {
	inv, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		devjwt.Problem(w, 404, "not found", "invoice "+r.PathValue("id"))
		return
	}
	if claims, cerr := devjwt.FromContext(r); cerr == nil && claims.TenantID != "" &&
		inv.TenantID != "" && inv.TenantID != claims.TenantID {
		devjwt.Problem(w, 404, "not found", "invoice "+r.PathValue("id"))
		return
	}
	if inv.IRN == "" {
		devjwt.Problem(w, 409, "not cleared", "invoice has no IRN yet — run pre-clearance first")
		return
	}
	payload, sig := QRPayload(inv)
	full := payload + "|" + sig
	matrix, err := QRMatrix([]byte(full))
	if err != nil {
		devjwt.Problem(w, 500, "qr error", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"invoice_id": inv.ID, "irn": inv.IRN,
		"payload": full, "signature": sig,
		"qr_svg":    QRSVG(matrix, 4),
		"algorithm": "HMAC-SHA256(truncated 12 hex) over NRS1|IRN|TIN|payableKobo|ts",
	})
}

// handlePreclear runs wf-mbs-preclearance for the invoice.
func (s *Server) handlePreclear(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.store.Get(id); !ok {
		devjwt.Problem(w, 404, "not found", "invoice "+id)
		return
	}
	run, err := s.runner.Run(r.Context(), s, "wf-mbs-preclearance", id)
	if err != nil {
		writeJSON(w, 422, map[string]any{"run": run, "error": err.Error()})
		return
	}
	inv, _ := s.store.Get(id)
	writeJSON(w, 200, map[string]any{"run": run, "invoice": inv})
}

// handleB2CReport is the B2C real-time reporter (no pre-clearance; realtime
// fiscalisation report to MBS via the tenant's APP).
func (s *Server) handleB2CReport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InvoiceID string            `json:"invoice_id"`
		Invoice   *CanonicalInvoice `json:"invoice"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		devjwt.Problem(w, 400, "bad request", err.Error())
		return
	}
	var inv *CanonicalInvoice
	if req.InvoiceID != "" {
		stored, ok := s.store.Get(req.InvoiceID)
		if !ok {
			devjwt.Problem(w, 404, "not found", "invoice "+req.InvoiceID)
			return
		}
		inv = stored
	} else if req.Invoice != nil {
		inv = req.Invoice
		inv.Normalise()
		inv.InvoiceType = "B2C"
		if inv.Status == "" {
			inv.Status = "received"
		}
		if _, err := s.store.Save(inv); err != nil {
			devjwt.Problem(w, 500, "persist failed", err.Error())
			return
		}
	} else {
		devjwt.Problem(w, 400, "bad request", "provide invoice_id or invoice")
		return
	}
	inv.InvoiceType = "B2C"
	receipt, appID, err := s.router.ReportB2C(r.Context(), inv)
	if err != nil {
		devjwt.Problem(w, 502, "mbs report failed", err.Error())
		return
	}
	inv.Status = "reported"
	_, _ = s.store.Save(inv)
	env, err2 := newInvoiceEvent("nrs.mbs.b2c.report.v1", inv)
	if err2 == nil {
		_ = s.outbox.Publish("nrs.mbs.b2c.report.v1", env)
	}
	writeJSON(w, 200, map[string]any{"receipt": receipt, "app": appID, "invoice_id": inv.ID})
}

// handleReplayList exposes the durable replay queue (outbox rows).
func (s *Server) handleReplayList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.outbox.Rows()
	if err != nil {
		devjwt.Problem(w, 500, "outbox error", err.Error())
		return
	}
	status := r.URL.Query().Get("status")
	out := rows
	if status != "" {
		out = nil
		for _, row := range rows {
			if row.Status == status {
				out = append(out, row)
			}
		}
	}
	writeJSON(w, 200, map[string]any{"rows": out, "count": len(out)})
}

// handleReplayOne re-publishes one outbox row (replay endpoint).
func (s *Server) handleReplayOne(w http.ResponseWriter, r *http.Request) {
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil {
		devjwt.Problem(w, 400, "bad seq", err.Error())
		return
	}
	rows, err := s.outbox.Rows()
	if err != nil {
		devjwt.Problem(w, 500, "outbox error", err.Error())
		return
	}
	for _, row := range rows {
		if row.Seq == seq {
			if err := s.outbox.Mark(seq, "pending", row.Attempts+1); err != nil {
				devjwt.Problem(w, 500, "mark failed", err.Error())
				return
			}
			writeJSON(w, 200, map[string]any{"replayed": seq, "topic": row.Topic})
			return
		}
	}
	devjwt.Problem(w, 404, "not found", "outbox seq "+r.PathValue("seq"))
}

func (s *Server) handleWorkflows(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"registered": []string{"wf-mbs-preclearance"},
		"runs":       s.runner.Runs(),
		"runner":     runnerKind(),
	})
}

func runnerKind() string {
	if os.Getenv("TEMPORAL_URL") != "" {
		return "temporal (via core temporal-sdkx)"
	}
	return "inproc (TEMPORAL_URL unset)"
}

var _ = strings.TrimSpace
