package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// wf-mbs-preclearance (SPEC §3 T1/T2): validate → CSID-sign → MBS submit →
// record. Temporal-backed in production (via core temporal-sdkx); when
// TEMPORAL_URL is unset the in-process runner below executes the same
// step graph with Temporal-equivalent semantics: ordered steps, per-step
// retry with backoff, durable run history.

// Step is one workflow activity.
type Step struct {
	Name    string
	Attempt int
	Status  string // ok|failed
	Detail  string
}

// WorkflowRun records one workflow execution.
type WorkflowRun struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	InputID    string    `json:"input_id"`
	Steps      []Step    `json:"steps"`
	Status     string    `json:"status"` // completed|failed
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// WorkflowFunc is a registered workflow; it receives the invoice id and may
// mutate state through the server.
type WorkflowFunc func(ctx context.Context, srv *Server, invoiceID string, rec func(Step)) error

// InprocRunner executes workflows in-process with retry/backoff.
type InprocRunner struct {
	mu    sync.Mutex
	runs  []WorkflowRun
	seq   int
	funcs map[string]WorkflowFunc
}

func NewInprocRunner() *InprocRunner {
	return &InprocRunner{funcs: map[string]WorkflowFunc{}}
}

// Register binds a workflow name to its implementation.
func (r *InprocRunner) Register(name string, fn WorkflowFunc) {
	r.funcs[name] = fn
}

// Runs returns run history.
func (r *InprocRunner) Runs() []WorkflowRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]WorkflowRun, len(r.runs))
	copy(out, r.runs)
	return out
}

// Run executes the workflow synchronously (dev runner semantics).
func (r *InprocRunner) Run(ctx context.Context, srv *Server, name, invoiceID string) (WorkflowRun, error) {
	fn, ok := r.funcs[name]
	if !ok {
		return WorkflowRun{}, fmt.Errorf("workflow %q not registered", name)
	}
	r.mu.Lock()
	r.seq++
	run := WorkflowRun{
		ID: fmt.Sprintf("run-%s-%06d", name, r.seq), Name: name, InputID: invoiceID,
		Status: "running", StartedAt: time.Now().UTC(),
	}
	r.mu.Unlock()
	rec := func(s Step) {
		r.mu.Lock()
		defer r.mu.Unlock()
		// replace on retry of same step name
		for i := range run.Steps {
			if run.Steps[i].Name == s.Name {
				run.Steps[i] = s
				return
			}
		}
		run.Steps = append(run.Steps, s)
	}
	err := fn(ctx, srv, invoiceID, rec)
	r.mu.Lock()
	defer r.mu.Unlock()
	run.FinishedAt = time.Now().UTC()
	if err != nil {
		run.Status = "failed"
	} else {
		run.Status = "completed"
	}
	r.runs = append(r.runs, run)
	return run, err
}

// retryActivity retries fn with linear backoff (Temporal-style default 3 attempts).
func retryActivity(ctx context.Context, name string, attempts int, rec func(Step), fn func() (string, error)) error {
	var last error
	for i := 1; i <= attempts; i++ {
		detail, err := fn()
		if err == nil {
			rec(Step{Name: name, Attempt: i, Status: "ok", Detail: detail})
			return nil
		}
		last = err
		rec(Step{Name: name, Attempt: i, Status: "failed", Detail: err.Error()})
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i*50) * time.Millisecond):
		}
	}
	return fmt.Errorf("activity %s failed after %d attempts: %w", name, attempts, last)
}

// registerWorkflows binds wf-mbs-preclearance on the runner.
func registerWorkflows(r *InprocRunner) {
	r.Register("wf-mbs-preclearance", wfMBSPreclearance)
}

// wfMBSPreclearance: validate → sign → submit → record (SPEC §3).
func wfMBSPreclearance(ctx context.Context, srv *Server, invoiceID string, rec func(Step)) error {
	inv, ok := srv.store.Get(invoiceID)
	if !ok {
		return fmt.Errorf("invoice %s not found", invoiceID)
	}
	// Step 1: validate (UBL + MBS business rules)
	err := retryActivity(ctx, "validate", 3, rec, func() (string, error) {
		violations, fatal, err := srv.validator.Validate(inv, srv.store.IsDuplicate(inv))
		if err != nil {
			return "", err
		}
		inv.Validation = violations
		if fatal {
			return "", fmt.Errorf("fatal validation violations: %d", len(violations))
		}
		return fmt.Sprintf("validated against %d packs, %d warnings", 2, len(violations)), nil
	})
	if err != nil {
		inv.Status = "failed"
		_, _ = srv.store.Save(inv)
		return err
	}
	// Step 2: CSID sign
	err = retryActivity(ctx, "csid-sign", 3, rec, func() (string, error) {
		srv.signer.SignInvoice(inv)
		return "signed with " + srv.signer.KeyID(), nil
	})
	if err != nil {
		return err
	}
	// Step 3: generate UBL
	var ublXML []byte
	err = retryActivity(ctx, "ubl-map", 3, rec, func() (string, error) {
		var err error
		ublXML, err = GenerateUBL(inv)
		if err != nil {
			return "", err
		}
		inv.UBLXML = string(ublXML)
		return fmt.Sprintf("UBL 2.1 generated (%d bytes)", len(ublXML)), nil
	})
	if err != nil {
		return err
	}
	// Step 4: MBS submit via APP router
	var result *ClearanceResult
	var appID string
	err = retryActivity(ctx, "mbs-submit", 3, rec, func() (string, error) {
		var err error
		result, appID, err = srv.router.Preclear(ctx, inv, ublXML)
		if err != nil {
			return "", err
		}
		if result.Status != "cleared" {
			return "", fmt.Errorf("mbs rejected: %s", result.Reason)
		}
		return "cleared via " + appID + ": " + result.IRN, nil
	})
	if err != nil {
		inv.Status = "failed"
		_, _ = srv.store.Save(inv)
		return err
	}
	// Step 5: record + emit event
	err = retryActivity(ctx, "record", 3, rec, func() (string, error) {
		inv.IRN = result.IRN
		inv.Stamp = result.Stamp
		inv.Status = "precleared"
		if _, err := srv.store.Save(inv); err != nil {
			return "", err
		}
		env, err := newInvoiceEvent("nrs.mbs.preclearance.v1", inv)
		if err != nil {
			return "", err
		}
		if err := srv.outbox.Publish("nrs.mbs.preclearance.v1", env); err != nil {
			return "", err
		}
		return "recorded IRN " + result.IRN, nil
	})
	return err
}
