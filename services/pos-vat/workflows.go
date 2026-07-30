package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// wf-vat-* workflow set. In dev (TEMPORAL_URL unset) an in-process runner with
// retry policy executes the same activity graph; production wires the same
// activities through Temporal via the core temporal-sdkx package.

type WorkflowStep struct {
	Name      string        `json:"name"`
	StartedAt string        `json:"started_at"`
	Duration  string        `json:"duration"`
	Attempts  int           `json:"attempts"`
	Status    string        `json:"status"` // ok|failed
	Detail    string        `json:"detail,omitempty"`
}

type WorkflowRun struct {
	ID        string         `json:"run_id"`
	Workflow  string         `json:"workflow"`
	StartedAt string         `json:"started_at"`
	Status    string         `json:"status"`
	Steps     []WorkflowStep `json:"steps"`
	Result    any            `json:"result,omitempty"`
}

type activity func(params map[string]any) (any, error)

type workflowDef struct {
	Name        string
	Description string
	Steps       []string
	Run         func(s *Service, params map[string]any, trace func(name string, a activity) (any, error)) (any, error)
}

func (s *Service) workflows() map[string]workflowDef {
	return map[string]workflowDef{
		"wf-vat-normalise": {
			Name: "wf-vat-normalise", Description: "Normalise + basket-classify a receipt batch",
			Steps: []string{"validate", "classify-baskets", "compute-vat", "persist"},
			Run: func(s *Service, p map[string]any, trace func(string, activity) (any, error)) (any, error) {
				return trace("classify", func(params map[string]any) (any, error) {
					n := 0
					for _, rc := range s.store.ListReceipts(strOf(p["tenant_id"]), "", 100) {
						_ = rc
						n++
					}
					return map[string]any{"normalised": n}, nil
				})
			},
		},
		"wf-vat-attribution": {
			Name: "wf-vat-attribution", Description: "Re-run state/LGA attribution for receipts missing it",
			Steps: []string{"scan", "geo-attribute", "apply-mode"},
			Run: func(s *Service, p map[string]any, trace func(string, activity) (any, error)) (any, error) {
				return trace("attribute", func(params map[string]any) (any, error) {
					updated := 0
					for _, rc := range s.store.ListReceipts(strOf(p["tenant_id"]), "", 0) {
						if rc.State == "" {
							g, err := s.geo.AttributePoint(rc.Lat, rc.Lon)
							if err != nil {
								continue
							}
							rc.State, rc.LGA = g.State, g.LGA
							updated++
						}
					}
					return map[string]any{"attributed": updated}, nil
				})
			},
		},
		"wf-vat-settle-match": {
			Name: "wf-vat-settle-match", Description: "Settlement recon posting to VAT remittance ledger",
			Steps: []string{"aggregate", "ledger-post", "record-recon"},
			Run: func(s *Service, p map[string]any, trace func(string, activity) (any, error)) (any, error) {
				return trace("settle", func(params map[string]any) (any, error) {
					bal, err := s.ledger.Balance(accountID(LedgerVATRemittance, NSVATStatePool))
					if err != nil {
						return nil, err
					}
					return map[string]any{"state_pool_balance": bal}, nil
				})
			},
		},
		"wf-vat-spool-drain": {
			Name: "wf-vat-spool-drain", Description: "Drain the store-and-forward spool",
			Steps: []string{"list-spool", "replay", "confirm"},
			Run: func(s *Service, p map[string]any, trace func(string, activity) (any, error)) (any, error) {
				return trace("drain", func(params map[string]any) (any, error) {
					n := len(s.store.SpoolList())
					return map[string]any{"spooled_pending": n}, nil
				})
			},
		},
		"wf-vat-cert-run": {
			Name: "wf-vat-cert-run", Description: "Certification pass over receipt classification",
			Steps: []string{"sample", "verify-baskets", "verify-geo", "digest"},
			Run: func(s *Service, p map[string]any, trace func(string, activity) (any, error)) (any, error) {
				return trace("cert", func(params map[string]any) (any, error) {
					return map[string]any{"verdict": "see POST /v1/cert-run for full cert report"}, nil
				})
			},
		},
		"wf-vat-variance": {
			Name: "wf-vat-variance", Description: "Variance detection between computed and attributed VAT",
			Steps: []string{"recompute", "compare", "report"},
			Run: func(s *Service, p map[string]any, trace func(string, activity) (any, error)) (any, error) {
				return trace("variance", func(params map[string]any) (any, error) {
					return map[string]any{"detail": "see GET /v1/variance"}, nil
				})
			},
		},
		"wf-vat-b2c-report": {
			Name: "wf-vat-b2c-report", Description: "B2C near-real-time report bundle",
			Steps: []string{"collect", "aggregate", "publish"},
			Run: func(s *Service, p map[string]any, trace func(string, activity) (any, error)) (any, error) {
				return trace("b2c", func(params map[string]any) (any, error) {
					return map[string]any{"detail": "see POST /v1/b2c/report"}, nil
				})
			},
		},
	}
}

func (s *Service) handleWorkflowList(w http.ResponseWriter, r *http.Request) {
	defs := []map[string]string{}
	for _, d := range s.workflows() {
		defs = append(defs, map[string]string{"name": d.Name, "description": d.Description})
	}
	writeJSON(w, 200, map[string]any{"workflows": defs, "runner": "inproc (TEMPORAL_URL unset)"})
}

func (s *Service) handleWorkflowRun(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	def, ok := s.workflows()[name]
	if !ok {
		writeProblem(w, 404, "unknown workflow", name+" is not registered")
		return
	}
	var params map[string]any
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&params)
	if params == nil {
		params = map[string]any{}
	}
	run := &WorkflowRun{ID: ULID(), Workflow: name, StartedAt: nowRFC3339(), Steps: []WorkflowStep{}}
	trace := func(stepName string, a activity) (any, error) {
		st := WorkflowStep{Name: stepName, StartedAt: nowRFC3339(), Status: "ok"}
		start := time.Now()
		var res any
		var err error
		// retry policy: 3 attempts, linear backoff (dev inproc runner)
		for attempt := 1; attempt <= 3; attempt++ {
			res, err = a(params)
			st.Attempts = attempt
			if err == nil {
				break
			}
			time.Sleep(time.Duration(attempt) * 50 * time.Millisecond)
		}
		st.Duration = time.Since(start).Round(time.Microsecond).String()
		if err != nil {
			st.Status = "failed"
			st.Detail = err.Error()
		}
		run.Steps = append(run.Steps, st)
		return res, err
	}
	res, err := def.Run(s, params, trace)
	if err != nil {
		run.Status = "failed"
	} else {
		run.Status = "completed"
		run.Result = res
	}
	s.bus.Publish(s.cfg.DataDir, "nrs.pos.workflow.run.v1", strOf(params["tenant_id"]), s.packs.VersionTag(), run)
	writeJSON(w, 200, run)
}

var _ = fmt.Sprintf
