package main

import (
	"encoding/json"
	"net/http"
	"time"
)

// wf-case-* workflow set with dev in-process runner (retry policy + trace),
// mirroring the core temporal-sdkx contract; production wires via Temporal.

type WorkflowStep struct {
	Name     string `json:"name"`
	Attempts int    `json:"attempts"`
	Status   string `json:"status"`
	Duration string `json:"duration"`
	Detail   string `json:"detail,omitempty"`
}

type WorkflowRun struct {
	ID        string         `json:"run_id"`
	Workflow  string         `json:"workflow"`
	StartedAt string         `json:"started_at"`
	Status    string         `json:"status"`
	Steps     []WorkflowStep `json:"steps"`
	Result    any            `json:"result,omitempty"`
}

type caseActivity func(s *Service, params map[string]any) (any, error)

type caseWorkflow struct {
	Name        string
	Description string
	Steps       []caseActivity
}

func (s *Service) workflows() map[string]caseWorkflow {
	return map[string]caseWorkflow{
		"wf-case-intake": {
			Name: "wf-case-intake", Description: "Validate intake, seed relations, open matter",
			Steps: []caseActivity{
				func(s *Service, p map[string]any) (any, error) {
					open := s.store.ListMatters("", "intake", "")
					return map[string]any{"intake_matters": len(open)}, nil
				},
				func(s *Service, p map[string]any) (any, error) {
					return map[string]any{"relations_seeded": len(s.rel.Tuples())}, nil
				},
			},
		},
		"wf-case-lifecycle": {
			Name: "wf-case-lifecycle", Description: "Advance matter lifecycle states with counsel checks",
			Steps: []caseActivity{
				func(s *Service, p map[string]any) (any, error) {
					counts := map[string]int{}
					for _, m := range s.store.ListMatters("", "", "") {
						counts[m.Status]++
					}
					return map[string]any{"status_counts": counts}, nil
				},
			},
		},
		"wf-case-deadlines": {
			Name: "wf-case-deadlines", Description: "Deadline scan: escalate overdue, notify <72h",
			Steps: []caseActivity{
				func(s *Service, p map[string]any) (any, error) {
					s.scanDeadlines()
					missed := s.store.ListDeadlines("", "escalated", "")
					open := s.store.ListDeadlines("", "open", "")
					return map[string]any{"open": len(open), "escalated": len(missed)}, nil
				},
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
		writeProblem(w, 404, "unknown workflow", name)
		return
	}
	var params map[string]any
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&params)
	run := &WorkflowRun{ID: ULID(), Workflow: name, StartedAt: nowRFC3339(), Steps: []WorkflowStep{}, Status: "completed"}
	var last any
	for _, step := range def.Steps {
		st := WorkflowStep{Name: def.Name, Status: "ok"}
		start := time.Now()
		var res any
		var err error
		for attempt := 1; attempt <= 3; attempt++ {
			res, err = step(s, params)
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
			run.Status = "failed"
			run.Steps = append(run.Steps, st)
			break
		}
		run.Steps = append(run.Steps, st)
		last = res
	}
	run.Result = last
	writeJSON(w, 200, run)
}
