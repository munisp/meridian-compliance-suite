package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// GateState is one reg-watch gate (carf.transmit_enabled, carf.gate.changed...).
type GateState struct {
	ID      string `json:"id"`
	Open    bool   `json:"open"`
	Source  string `json:"source"` // reg-watch|local-file|default
	Note    string `json:"note,omitempty"`
	Updated string `json:"updated"`
}

// GateChecker resolves gates from the core reg-watch API with a local gate
// file fallback (data/gates.json) when reg-watch is unreachable (SPEC §1.3).
type GateChecker struct {
	regWatchURL string
	gateFile    string
	mu          sync.Mutex
	cached      map[string]GateState
	cachedAt    time.Time
}

func NewGateChecker(regWatchURL, dataDir string) *GateChecker {
	return &GateChecker{regWatchURL: regWatchURL, gateFile: filepath.Join(dataDir, "gates.json"), cached: map[string]GateState{}}
}

func (g *GateChecker) refresh() map[string]GateState {
	out := map[string]GateState{}
	if g.regWatchURL != "" {
		cli := &http.Client{Timeout: 1500 * time.Millisecond}
		resp, err := cli.Get(g.regWatchURL + "/v1/gates")
		if err == nil && resp.StatusCode == 200 {
			var payload struct {
				Gates []struct {
					ID   string `json:"id"`
					Open bool   `json:"open"`
					Note string `json:"note"`
				} `json:"gates"`
			}
			if json.NewDecoder(resp.Body).Decode(&payload) == nil {
				for _, gg := range payload.Gates {
					out[gg.ID] = GateState{ID: gg.ID, Open: gg.Open, Source: "reg-watch", Note: gg.Note, Updated: nowRFC3339()}
				}
			}
			resp.Body.Close()
		} else if err == nil {
			resp.Body.Close()
		}
	}
	// local gate file fallback / override layer
	if b, err := os.ReadFile(g.gateFile); err == nil {
		var local map[string]bool
		if json.Unmarshal(b, &local) == nil {
			for id, open := range local {
				if _, fromReg := out[id]; !fromReg {
					out[id] = GateState{ID: id, Open: open, Source: "local-file", Updated: nowRFC3339()}
				}
			}
		}
	}
	// defaults for gates never seen: closed (fail-safe)
	for _, id := range []string{"carf.transmit_enabled", "carf.gate.changed"} {
		if _, ok := out[id]; !ok {
			out[id] = GateState{ID: id, Open: false, Source: "default", Note: "fail-safe closed", Updated: nowRFC3339()}
		}
	}
	return out
}

func (g *GateChecker) Gates() map[string]GateState {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.cachedAt) > 10*time.Second {
		g.cached = g.refresh()
		g.cachedAt = time.Now()
	}
	out := map[string]GateState{}
	for k, v := range g.cached {
		out[k] = v
	}
	return out
}

// Open reports whether a gate is open. Unknown gates are closed (fail-safe).
func (g *GateChecker) Open(id string) bool {
	gs := g.Gates()
	if st, ok := gs[id]; ok {
		return st.Open
	}
	return false
}

// SetLocal writes a local gate override (dev board-authorized flip).
func (g *GateChecker) SetLocal(id string, open bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	local := map[string]bool{}
	if b, err := os.ReadFile(g.gateFile); err == nil {
		json.Unmarshal(b, &local)
	}
	local[id] = open
	b, _ := json.MarshalIndent(local, "", "  ")
	if err := os.WriteFile(g.gateFile, b, 0o644); err != nil {
		return err
	}
	g.cachedAt = time.Time{}
	return nil
}
