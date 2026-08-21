package main

// nrs_resume.go — interrupted e-invoice recovery (audit FF-6).
//
// The in-process workflow runner is synchronous: a process crash mid-flow
// leaves the invoice durably persisted in a non-terminal state ("received",
// "signed", "transmitted") with no run history to resume from. Before this
// fix, a client retry with the same IRN / Idempotency-Key returned the stale
// record WITHOUT re-driving the lifecycle, and no sweeper existed — the
// invoice never reached "confirmed" without manual re-drive.
//
// The fix has two parts, both built on runNRSWorkflow:
//
//  1. Resume-on-retry: the idempotent-replay paths in handleNRSCreate
//     re-drive wf-nrs-einvoice when the stored invoice is mid-flow instead
//     of returning the stale record.
//  2. Recovery sweep: RecoverInterruptedNRS scans the durable store for
//     mid-flow NRS invoices and re-drives them (wired at boot + ticker in
//     main).
//
// No-double-issuance invariant: the IRN is deterministic
// (InvoiceNumber-ServiceID-YYYYMMDD, uniqueness-checked at step 3) and every
// lifecycle step is idempotent on the durable record, so re-driving a
// mid-flow invoice can never issue a second IRN or a duplicate invoice.
// Exactly-once confirmation: runNRSWorkflow serializes all drivers (handler,
// retry-resume, sweeper) through a per-invoice in-flight guard, and step 8
// ("8-confirm") is a single durable status transition to "confirmed" that is
// skipped once terminal.

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// nrsInterimStatus reports whether the invoice is an NRS-flow invoice durably
// persisted in a non-terminal mid-flow state (crash between the draft save
// and step 8). "failed" is NOT interim: it is the explicit outcome after the
// step retries are exhausted (and existing replay semantics return it
// unchanged); recovery from "failed" stays a manual re-drive.
func nrsInterimStatus(inv *CanonicalInvoice) bool {
	if inv == nil || inv.NRSPayload == "" {
		return false // not ingested via the NRS lifecycle
	}
	switch inv.Status {
	case "received", "signed", "transmitted":
		return true
	}
	return false
}

// runNRSWorkflow executes wf-nrs-einvoice under the per-invoice in-flight
// guard. When another driver (handler, retry-resume, or sweeper) is already
// running the workflow for this invoice, it returns started=false and the
// caller serves the current durable record — the active run owns the status
// transition. This is what makes "confirmed" exactly-once under concurrent
// retries/sweeps.
func (s *Server) runNRSWorkflow(ctx context.Context, invoiceID string) (run WorkflowRun, started bool, err error) {
	s.resumeMu.Lock()
	if s.resumeInFlight == nil {
		s.resumeInFlight = map[string]bool{}
	}
	if s.resumeInFlight[invoiceID] {
		s.resumeMu.Unlock()
		return WorkflowRun{}, false, nil
	}
	s.resumeInFlight[invoiceID] = true
	s.resumeMu.Unlock()
	defer func() {
		s.resumeMu.Lock()
		delete(s.resumeInFlight, invoiceID)
		s.resumeMu.Unlock()
	}()
	run, err = s.runner.Run(ctx, s, "wf-nrs-einvoice", invoiceID)
	return run, true, err
}

// resumeInterrupted re-drives the workflow for a mid-flow invoice hit by an
// idempotent replay, writing the post-resume response. It reports false when
// another driver already holds the in-flight guard — the caller then serves
// the current durable record exactly as a plain replay.
func (s *Server) resumeInterrupted(w http.ResponseWriter, r *http.Request, invoiceID string) bool {
	run, started, err := s.runNRSWorkflow(r.Context(), invoiceID)
	if !started {
		return false
	}
	stored, _ := s.store.Get(invoiceID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(422)
		resp := nrsResponse(s, stored, &run, true)
		resp["error"] = err.Error()
		_ = json.NewEncoder(w).Encode(resp)
		return true
	}
	writeJSON(w, 200, nrsResponse(s, stored, &run, true))
	return true
}

// RecoverInterruptedNRS re-drives every NRS-flow invoice durably stuck in a
// mid-flow state (process crash between durable saves). It is safe to call
// concurrently with request traffic: invoices whose workflow is currently
// running are skipped by the in-flight guard, and terminal invoices
// (confirmed/failed) are never touched. Returns the number of resumes
// started.
func (s *Server) RecoverInterruptedNRS(ctx context.Context) int {
	resumed := 0
	for _, inv := range s.store.List() {
		if !nrsInterimStatus(inv) {
			continue
		}
		run, started, err := s.runNRSWorkflow(ctx, inv.ID)
		if !started {
			continue // already being driven by an in-flight request
		}
		resumed++
		if err != nil {
			log.Printf("einvoice recovery: invoice %s (irn %s) resume failed: %v (run %s)", inv.ID, inv.IRN, err, run.ID)
			continue
		}
		log.Printf("einvoice recovery: invoice %s (irn %s) resumed to completion (run %s)", inv.ID, inv.IRN, run.ID)
	}
	return resumed
}

// startNRSRecoverySweep runs an immediate recovery pass at boot and then a
// periodic sweep, so a crash mid-flow self-heals without client action.
func (s *Server) startNRSRecoverySweep(ctx context.Context, interval time.Duration) {
	if n := s.RecoverInterruptedNRS(ctx); n > 0 {
		log.Printf("einvoice recovery: resumed %d interrupted invoice(s) at boot", n)
	}
	go func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				s.RecoverInterruptedNRS(ctx)
			}
		}
	}()
}
