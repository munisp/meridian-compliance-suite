package main

// NRS-parity HTTP endpoints:
//
//	POST  /v1/invoices/nrs        NRS-schema ingestion -> full 8-step lifecycle
//	PATCH /v1/invoices/{irn}      payment_status / reference update (only)
//	POST  /v1/webhooks            stakeholder webhook registration
//	GET   /v1/webhooks            registered endpoints + delivery history
//
// Errors use the repo RFC7807 pattern (devjwt.Problem); NRS schema validation
// failures additionally carry the full NRS-style error list.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/munisp/meridian-compliance-suite/packages/shared/devjwt"
)

// nrsErrorProblem writes an RFC7807 problem with the NRS-style error list
// embedded as an extension member.
func nrsErrorProblem(w http.ResponseWriter, status int, title string, errs NRSErrors) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "about:blank", "title": title, "status": status,
		"detail": errs.Error(), "errors": errs,
	})
}

// nrsResponse is the NRS-style lifecycle response.
func nrsResponse(srv *Server, inv *CanonicalInvoice, run *WorkflowRun, idempotent bool) map[string]any {
	out := map[string]any{
		"irn":            inv.IRN,
		"status":         inv.Status,
		"invoice":        ToNRS(inv),
		"crypto_stamp":   inv.Stamp,
		"invoice_id":     inv.ID,
		"payment_status": inv.PaymentStatus,
	}
	if run != nil {
		out["run_id"] = run.ID
		out["steps"] = run.Steps
	}
	if idempotent {
		out["idempotent_replay"] = true
	}
	if inv.IRN != "" {
		payload, sig := QRPayload(inv)
		full := payload + "|" + sig
		if matrix, err := QRMatrix([]byte(full)); err == nil {
			out["qr"] = map[string]any{
				"payload": full, "signature": sig, "qr_svg": QRSVG(matrix, 4),
			}
		}
	}
	return out
}

// handleNRSCreate ingests an NRS-schema payload and runs the 8-step lifecycle.
// A payload carrying an IRN skips generation; resubmission with the same IRN
// is idempotent (returns the existing confirmed invoice, never a duplicate).
func (s *Server) handleNRSCreate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		devjwt.Problem(w, 413, "payload too large", err.Error())
		return
	}
	var n NRSInvoice
	if err := json.Unmarshal(body, &n); err != nil {
		devjwt.Problem(w, 400, "bad request", "invalid NRS payload: "+err.Error())
		return
	}
	// Idempotent resubmission: same IRN -> same invoice, no duplicate.
	if n.IRN != "" {
		if existing, ok := s.store.GetByIRN(strings.TrimSpace(n.IRN)); ok {
			// FF-6: a mid-flow record (crash before confirmation) is RESUMED,
			// not returned stale; terminal records replay as before.
			if nrsInterimStatus(existing) && s.resumeInterrupted(w, r, existing.ID) {
				return
			}
			writeJSON(w, 200, nrsResponse(s, existing, nil, true))
			return
		}
	}
	inv, err := FromNRS(&n)
	if err != nil {
		if errs, ok := err.(NRSErrors); ok {
			nrsErrorProblem(w, 422, "NRS schema validation failed", errs)
			return
		}
		devjwt.Problem(w, 422, "conversion failed", err.Error())
		return
	}
	claims, _ := devjwt.FromContext(r)
	inv.TenantID = claims.TenantID
	inv.IdempotencyKey = r.Header.Get("Idempotency-Key")
	inv.NRSPayload = string(body)
	inv.Normalise()
	if inv.PaymentStatus == "" {
		inv.PaymentStatus = "PENDING"
	}
	if priorID, err := s.store.Save(inv); priorID != "" {
		if err != nil && !errors.Is(err, ErrIdempotentReplay) {
			// same key + different payload -> 409, not a silent replay
			writeStoreConflict(w, err)
			return
		}
		prior, _ := s.store.Get(priorID)
		// FF-6: resume a mid-flow record on idempotency-key replay.
		if nrsInterimStatus(prior) && s.resumeInterrupted(w, r, prior.ID) {
			return
		}
		writeJSON(w, 200, nrsResponse(s, prior, nil, true))
		return
	} else if err != nil {
		writeStoreConflict(w, err)
		return
	}
	// Guarded run (FF-6): if the recovery sweep is already driving this
	// invoice, serve the current durable record instead of double-driving.
	run, started, err := s.runNRSWorkflow(r.Context(), inv.ID)
	if !started {
		stored, _ := s.store.Get(inv.ID)
		writeJSON(w, 202, nrsResponse(s, stored, nil, false))
		return
	}
	if err != nil {
		stored, _ := s.store.Get(inv.ID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(422)
		resp := nrsResponse(s, stored, &run, false)
		resp["error"] = err.Error()
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	stored, _ := s.store.Get(inv.ID)
	writeJSON(w, 201, nrsResponse(s, stored, &run, false))
}

// handleNRSUpdate is the PATCH-by-IRN endpoint: ONLY payment_status
// (PENDING|PAID|REJECTED) and reference are mutable after signage; any other
// field in the body is a locked-field mutation attempt -> 409.
func (s *Server) handleNRSUpdate(w http.ResponseWriter, r *http.Request) {
	irn := r.PathValue("irn")
	inv, ok := s.store.GetByIRN(irn)
	if !ok {
		devjwt.Problem(w, 404, "not found", "invoice with irn "+irn)
		return
	}
	if claims, ok := devjwt.FromContext(r); ok && claims.TenantID != "" &&
		inv.TenantID != "" && inv.TenantID != claims.TenantID {
		devjwt.Problem(w, 404, "not found", "invoice with irn "+irn)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		devjwt.Problem(w, 413, "payload too large", err.Error())
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		devjwt.Problem(w, 400, "bad request", err.Error())
		return
	}
	for k := range raw {
		if k != "payment_status" && k != "reference" {
			devjwt.Problem(w, 409, "field locked",
				"field "+k+" is immutable after invoice signage; only payment_status and reference are mutable")
			return
		}
	}
	var req struct {
		PaymentStatus string `json:"payment_status"`
		Reference     string `json:"reference"`
	}
	_ = json.Unmarshal(body, &req)
	if req.PaymentStatus == "" && req.Reference == "" {
		devjwt.Problem(w, 400, "bad request", "provide payment_status and/or reference")
		return
	}
	if req.PaymentStatus != "" {
		if !ValidPaymentStatus(req.PaymentStatus) {
			devjwt.Problem(w, 422, "invalid payment_status", "payment_status must be PENDING|PAID|REJECTED")
			return
		}
		inv.PaymentStatus = strings.ToUpper(req.PaymentStatus)
	}
	if req.Reference != "" {
		inv.PaymentReference = req.Reference
	}
	actor := ""
	if claims, ok := devjwt.FromContext(r); ok {
		actor = claims.Sub
	}
	inv.Audit = append(inv.Audit, AuditEntry{
		At: time.Now().UTC().Format(time.RFC3339), Action: "payment_status_update",
		Detail: "payment_status=" + inv.PaymentStatus + " reference=" + inv.PaymentReference,
		Actor:  actor,
	})
	if _, err := s.store.Save(inv); err != nil {
		devjwt.Problem(w, 500, "persist failed", err.Error())
		return
	}
	if env, err := newInvoiceEvent("nrs.einvoice.payment_status.v1", inv); err == nil {
		_ = s.outbox.Publish("nrs.einvoice.payment_status.v1", env)
	}
	writeJSON(w, 200, map[string]any{
		"irn": inv.IRN, "payment_status": inv.PaymentStatus,
		"reference": inv.PaymentReference, "invoice": ToNRS(inv),
	})
}

// hasAnyRole reports whether the principal holds any of the given roles.
func hasAnyRole(have []string, want ...string) bool {
	for _, h := range have {
		for _, w2 := range want {
			if h == w2 {
				return true
			}
		}
	}
	return false
}

// handleWebhookRegister registers a stakeholder webhook for a business.
// A1-05: ownership + role gated — the caller must be an authenticated
// admin/operator whose tenant owns the business (first registration binds
// the business to the tenant; the invoice store is cross-checked so a
// tenant can never register callbacks for another tenant's business).
func (s *Server) handleWebhookRegister(w http.ResponseWriter, r *http.Request) {
	claims, ok := devjwt.FromContext(r)
	if !ok || claims.Sub == "" {
		devjwt.Problem(w, 401, "unauthorized", "authentication required")
		return
	}
	if !hasAnyRole(claims.Roles, "admin", "operator") {
		devjwt.Problem(w, 403, "forbidden", "admin or operator role required")
		return
	}
	if claims.TenantID == "" {
		devjwt.Problem(w, 403, "forbidden", "tenant claim required")
		return
	}
	var req struct {
		BusinessID string `json:"business_id"`
		URL        string `json:"url"`
		Secret     string `json:"secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		devjwt.Problem(w, 400, "bad request", err.Error())
		return
	}
	// BOLA cross-check against durable invoice data: any existing invoice
	// for this business owned by another tenant denies registration.
	for _, inv := range s.store.List() {
		if inv.BusinessID == req.BusinessID && inv.TenantID != "" && inv.TenantID != claims.TenantID {
			devjwt.Problem(w, 403, "forbidden", "business is owned by another tenant")
			return
		}
	}
	if err := s.webhooks.Register(req.BusinessID, claims.TenantID, req.URL, req.Secret); err != nil {
		status := 422
		if strings.Contains(err.Error(), "owned by another tenant") {
			status = 403
		}
		devjwt.Problem(w, status, "webhook registration failed", err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"registered": req.URL, "business_id": req.BusinessID})
}

// handleWebhookList lists registered endpoints (secrets redacted) and
// delivery history. A1-05: tenant-scoped — only the owning tenant may list.
func (s *Server) handleWebhookList(w http.ResponseWriter, r *http.Request) {
	claims, ok := devjwt.FromContext(r)
	if !ok || claims.Sub == "" {
		devjwt.Problem(w, 401, "unauthorized", "authentication required")
		return
	}
	businessID := r.URL.Query().Get("business_id")
	if businessID != "" {
		owner := s.webhooks.Owner(businessID)
		if owner != "" && owner != claims.TenantID {
			devjwt.Problem(w, 403, "forbidden", "business is owned by another tenant")
			return
		}
		if owner == "" && claims.TenantID == "" {
			devjwt.Problem(w, 403, "forbidden", "tenant claim required")
			return
		}
	}
	writeJSON(w, 200, map[string]any{
		"endpoints":  s.webhooks.Endpoints(businessID),
		"deliveries": s.webhooks.Deliveries(),
	})
}
