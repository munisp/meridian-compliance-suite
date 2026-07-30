// Package rulepack loads and evaluates rp-* YAML rule packs (SPEC §1.4) and
// provides a client for the core rules-engine API (POST /v1/evaluate) with a
// local-embedded fallback so services run dev-standalone.
package rulepack

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed packs
var embedded embed.FS

// Pack is one rp-* rule-pack document (SPEC §1.4).
type Pack struct {
	ID                 string         `yaml:"id" json:"id"`
	Version            string         `yaml:"version" json:"version"`
	EffectiveFrom      string         `yaml:"effective_from" json:"effective_from"`
	EffectiveTo        *string        `yaml:"effective_to" json:"effective_to"`
	Status             string         `yaml:"status" json:"status"`
	SubjectToRegazette bool           `yaml:"subject_to_regazette" json:"subject_to_regazette"`
	Provenance         map[string]any `yaml:"provenance" json:"provenance"`
	Signed             map[string]any `yaml:"signed" json:"signed"`
	Rules              []Rule         `yaml:"rules" json:"rules"`
}

// Rule: when-clauses (context predicates) → then-effects (decision payload).
type Rule struct {
	ID   string         `yaml:"id" json:"id"`
	When map[string]any `yaml:"when" json:"when"`
	Then map[string]any `yaml:"then" json:"then"`
}

// Ref identifies a pack version, e.g. "rp-wht-2024@1.0.0".
func (p *Pack) Ref() string { return p.ID + "@" + p.Version }

// Parse decodes one pack YAML document.
func Parse(data []byte) (*Pack, error) {
	var p Pack
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse pack: %w", err)
	}
	if p.ID == "" || p.Version == "" {
		return nil, fmt.Errorf("pack missing id/version")
	}
	return &p, nil
}

// Load reads packs/<id>/<version>.yaml from dir; version "" = latest file.
func Load(dir, id, version string) (*Pack, error) {
	if version == "" {
		matches, err := filepath.Glob(filepath.Join(dir, "packs", id, "*.yaml"))
		if err != nil || len(matches) == 0 {
			return nil, fmt.Errorf("no versions of %s in %s", id, dir)
		}
		version = strings.TrimSuffix(filepath.Base(matches[len(matches)-1]), ".yaml")
	}
	data, err := os.ReadFile(filepath.Join(dir, "packs", id, version+".yaml"))
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// LoadEmbedded reads a pack from the embedded fallback copies.
func LoadEmbedded(id, version string) (*Pack, error) {
	if version == "" {
		entries, err := fs.ReadDir(embedded, "packs/"+id)
		if err != nil || len(entries) == 0 {
			return nil, fmt.Errorf("no embedded pack %s", id)
		}
		version = strings.TrimSuffix(entries[len(entries)-1].Name(), ".yaml")
	}
	data, err := embedded.ReadFile("packs/" + id + "/" + version + ".yaml")
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// TraceEntry records how one rule fired (or why not).
type TraceEntry struct {
	RuleID  string `json:"rule_id"`
	Matched bool   `json:"matched"`
	Reason  string `json:"reason,omitempty"`
}

// Decision is the merged outcome of evaluating a pack against a context.
type Decision struct {
	Pack  string         `json:"pack"`
	Attrs map[string]any `json:"decision"`
	Trace []TraceEntry   `json:"trace"`
}

// toFloat normalises YAML/JSON numerics.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	}
	return 0, false
}

// matchCond evaluates one when-clause key. Operator suffixes: __in, __ne,
// __lt, __lte, __gt, __gte, __exists.
func matchCond(key string, want any, ctx map[string]any) (bool, string) {
	base, op := key, "eq"
	for _, o := range []string{"__in", "__ne", "__lte", "__lt", "__gte", "__gt", "__exists"} {
		if strings.HasSuffix(key, o) {
			base, op = strings.TrimSuffix(key, o), strings.TrimPrefix(o, "__")
			break
		}
	}
	got, ok := ctx[base]
	switch op {
	case "exists":
		wantB, _ := want.(bool)
		return (ok == wantB) || want == nil, fmt.Sprintf("%s exists=%v", base, ok)
	case "in":
		list, _ := want.([]any)
		for _, item := range list {
			if fmt.Sprint(item) == fmt.Sprint(got) {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%s=%v not in %v", base, got, want)
	case "eq":
		if fmt.Sprint(got) == fmt.Sprint(want) {
			return true, ""
		}
		if gf, gok := toFloat(got); gok {
			if wf, wok := toFloat(want); wok && gf == wf {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%s=%v != %v", base, got, want)
	case "ne":
		m, _ := matchCond(base, want, ctx)
		return !m, fmt.Sprintf("%s ne %v", base, want)
	default: // numeric comparisons
		gf, gok := toFloat(got)
		wf, wok := toFloat(want)
		if !gok || !wok {
			return false, fmt.Sprintf("%s not numeric", base)
		}
		var m bool
		switch op {
		case "lt":
			m = gf < wf
		case "lte":
			m = gf <= wf
		case "gt":
			m = gf > wf
		case "gte":
			m = gf >= wf
		}
		if !m {
			return false, fmt.Sprintf("%s=%v fails %s %v", base, gf, op, wf)
		}
		return true, ""
	}
}

// Match reports whether all when-clauses hold for the context.
func (r *Rule) Match(ctx map[string]any) (bool, string) {
	for k, want := range r.When {
		ok, why := matchCond(k, want, ctx)
		if !ok {
			return false, why
		}
	}
	return true, ""
}

// Evaluate runs every rule and merges the `then` payloads of matching rules.
func Evaluate(p *Pack, ctx map[string]any) *Decision {
	d := &Decision{Pack: p.Ref(), Attrs: map[string]any{}}
	for i := range p.Rules {
		r := &p.Rules[i]
		ok, why := r.Match(ctx)
		d.Trace = append(d.Trace, TraceEntry{RuleID: r.ID, Matched: ok, Reason: why})
		if ok {
			for k, v := range r.Then {
				d.Attrs[k] = v
			}
		}
	}
	return d
}

// EngineClient calls the core rules-engine (SPEC §2) when reachable.
type EngineClient struct {
	BaseURL string // e.g. http://localhost:8020; empty disables
	Client  *http.Client
}

func (c *EngineClient) http() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 3 * time.Second}
}

// Evaluate POSTs /v1/evaluate {pack_id, version, context}.
func (c *EngineClient) Evaluate(packID, version string, ctx map[string]any) (*Decision, error) {
	if c.BaseURL == "" {
		return nil, fmt.Errorf("rules-engine base url empty")
	}
	body, _ := json.Marshal(map[string]any{"pack_id": packID, "version": version, "context": ctx})
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/v1/evaluate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rules-engine status %d", resp.StatusCode)
	}
	var out struct {
		Decision map[string]any `json:"decision"`
		Trace    []TraceEntry   `json:"trace"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &Decision{Pack: packID + "@" + version, Attrs: out.Decision, Trace: out.Trace}, nil
}

// Evaluator tries the rules-engine first, then RP_PACKS_DIR, then embedded.
type Evaluator struct {
	Engine   *EngineClient
	PacksDir string // override dir containing packs/<id>/<version>.yaml
}

func (e *Evaluator) Evaluate(packID, version string, ctx map[string]any) (*Decision, error) {
	if e.Engine != nil && e.Engine.BaseURL != "" {
		if d, err := e.Engine.Evaluate(packID, version, ctx); err == nil {
			return d, nil
		}
	}
	p, err := e.loadLocal(packID, version)
	if err != nil {
		return nil, err
	}
	return Evaluate(p, ctx), nil
}

// Load returns the pack document (dir → embedded fallback).
func (e *Evaluator) loadLocal(packID, version string) (*Pack, error) {
	if e.PacksDir != "" {
		if p, err := Load(e.PacksDir, packID, version); err == nil {
			return p, nil
		}
	}
	return LoadEmbedded(packID, version)
}

// LoadPack returns the pack document via the fallback chain.
func (e *Evaluator) LoadPack(packID, version string) (*Pack, error) {
	return e.loadLocal(packID, version)
}
