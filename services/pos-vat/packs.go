package main

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// ---------- minimal YAML-subset parser (offline-resilient; no external deps) ----------
// Supports: nested block maps, block lists ("- "), scalars (null/bool/int/float/
// quoted/plain strings), inline flow maps {a: b} and flow lists [a, b], comments.
// Sufficient for the rp-* pack grammar in SPEC §1.4.

type yamlLine struct {
	indent  int
	content string
}

func ParseYAML(doc string) (any, error) {
	var lines []yamlLine
	for _, raw := range strings.Split(doc, "\n") {
		if strings.TrimSpace(raw) == "" || strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}
		noComment := stripComment(raw)
		if strings.TrimSpace(noComment) == "" {
			continue
		}
		indent := len(noComment) - len(strings.TrimLeft(noComment, " "))
		lines = append(lines, yamlLine{indent, strings.TrimSpace(noComment)})
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("empty document")
	}
	v, n, err := parseBlock(lines, 0, lines[0].indent)
	if err != nil {
		return nil, err
	}
	if n != len(lines) {
		return nil, fmt.Errorf("trailing content at line %d", n)
	}
	return v, nil
}

func stripComment(s string) string {
	inS, inD := false, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inD {
				inS = !inS
			}
		case '"':
			if !inS {
				inD = !inD
			}
		case '#':
			if !inS && !inD && (i == 0 || s[i-1] == ' ') {
				return s[:i]
			}
		}
	}
	return s
}

func parseBlock(lines []yamlLine, i, indent int) (any, int, error) {
	if i >= len(lines) {
		return nil, i, nil
	}
	if strings.HasPrefix(lines[i].content, "- ") || lines[i].content == "-" {
		return parseList(lines, i, indent)
	}
	return parseMap(lines, i, indent)
}

func parseMap(lines []yamlLine, i, indent int) (any, int, error) {
	m := map[string]any{}
	for i < len(lines) {
		ln := lines[i]
		if ln.indent < indent {
			break
		}
		if ln.indent > indent {
			return nil, i, fmt.Errorf("unexpected indent at %q", ln.content)
		}
		if strings.HasPrefix(ln.content, "- ") {
			break
		}
		key, val, ok := splitKV(ln.content)
		if !ok {
			return nil, i, fmt.Errorf("bad map line %q", ln.content)
		}
		i++
		if val != "" {
			v, err := parseScalar(val)
			if err != nil {
				return nil, i, err
			}
			m[key] = v
		} else {
			// nested block or null
			if i < len(lines) && lines[i].indent > indent {
				v, ni, err := parseBlock(lines, i, lines[i].indent)
				if err != nil {
					return nil, ni, err
				}
				m[key] = v
				i = ni
			} else if i < len(lines) && lines[i].indent == indent && (strings.HasPrefix(lines[i].content, "- ") || lines[i].content == "-") {
				v, ni, err := parseList(lines, i, indent)
				if err != nil {
					return nil, ni, err
				}
				m[key] = v
				i = ni
			} else {
				m[key] = nil
			}
		}
	}
	return m, i, nil
}

func parseList(lines []yamlLine, i, indent int) (any, int, error) {
	var out []any
	for i < len(lines) {
		ln := lines[i]
		if ln.indent < indent || (!strings.HasPrefix(ln.content, "- ") && ln.content != "-") {
			break
		}
		if ln.indent > indent {
			return nil, i, fmt.Errorf("unexpected indent in list at %q", ln.content)
		}
		item := strings.TrimSpace(strings.TrimPrefix(ln.content, "-"))
		i++
		if item == "" {
			if i < len(lines) && lines[i].indent > indent {
				v, ni, err := parseBlock(lines, i, lines[i].indent)
				if err != nil {
					return nil, ni, err
				}
				out = append(out, v)
				i = ni
			} else {
				out = append(out, nil)
			}
			continue
		}
		if key, val, ok := splitKV(item); ok && !strings.HasPrefix(item, "{") && !strings.HasPrefix(item, "[") {
			// inline map start: treat rest as map entries at deeper indent
			m := map[string]any{}
			if val != "" {
				v, err := parseScalar(val)
				if err != nil {
					return nil, i, err
				}
				m[key] = v
			} else if i < len(lines) && lines[i].indent > indent {
				v, ni, err := parseBlock(lines, i, lines[i].indent)
				if err != nil {
					return nil, ni, err
				}
				m[key] = v
				i = ni
			} else {
				m[key] = nil
			}
			// consume following deeper-indented map lines
			for i < len(lines) && lines[i].indent > indent && !strings.HasPrefix(lines[i].content, "- ") {
				sub := lines[i]
				k2, v2, ok2 := splitKV(sub.content)
				if !ok2 {
					return nil, i, fmt.Errorf("bad map line %q", sub.content)
				}
				i++
				if v2 != "" {
					v, err := parseScalar(v2)
					if err != nil {
						return nil, i, err
					}
					m[k2] = v
				} else if i < len(lines) && lines[i].indent > sub.indent {
					v, ni, err := parseBlock(lines, i, lines[i].indent)
					if err != nil {
						return nil, ni, err
					}
					m[k2] = v
					i = ni
				} else {
					m[k2] = nil
				}
			}
			out = append(out, m)
			continue
		}
		v, err := parseScalar(item)
		if err != nil {
			return nil, i, err
		}
		out = append(out, v)
	}
	return out, i, nil
}

func splitKV(s string) (key, val string, ok bool) {
	idx := strings.Index(s, ": ")
	if idx < 0 {
		if strings.HasSuffix(s, ":") {
			return strings.TrimSpace(s[:len(s)-1]), "", true
		}
		return "", "", false
	}
	return strings.TrimSpace(s[:idx]), strings.TrimSpace(s[idx+2:]), true
}

func parseScalar(s string) (any, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "null" || s == "~":
		return nil, nil
	case s == "true":
		return true, nil
	case s == "false":
		return false, nil
	case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"):
		m := map[string]any{}
		body := strings.TrimSpace(s[1 : len(s)-1])
		if body == "" {
			return m, nil
		}
		for _, part := range splitFlow(body) {
			k, v, ok := splitKV(part)
			if !ok {
				return nil, fmt.Errorf("bad flow map entry %q", part)
			}
			vv, err := parseScalar(v)
			if err != nil {
				return nil, err
			}
			m[k] = vv
		}
		return m, nil
	case strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]"):
		var out []any
		body := strings.TrimSpace(s[1 : len(s)-1])
		if body == "" {
			return out, nil
		}
		for _, part := range splitFlow(body) {
			vv, err := parseScalar(strings.TrimSpace(part))
			if err != nil {
				return nil, err
			}
			out = append(out, vv)
		}
		return out, nil
	case strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") && len(s) >= 2:
		return s[1 : len(s)-1], nil
	case strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'") && len(s) >= 2:
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	}
	if iv, err := strconv.ParseInt(s, 10, 64); err == nil {
		return iv, nil
	}
	if fv, err := strconv.ParseFloat(s, 64); err == nil {
		return fv, nil
	}
	return s, nil
}

func splitFlow(body string) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(body[start:i]))
				start = i + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(body[start:]))
	return parts
}

// ---------- rp-* pack model (SPEC §1.4) ----------

type Rule struct {
	ID   string         `json:"id"`
	When map[string]any `json:"when"`
	Then map[string]any `json:"then"`
}

type Pack struct {
	ID                 string         `json:"id"`
	Version            string         `json:"version"`
	EffectiveFrom      string         `json:"effective_from"`
	Status             string         `json:"status"`
	SubjectToRegazette bool           `json:"subject_to_regazette"`
	Provenance         map[string]any `json:"provenance"`
	Rules              []Rule         `json:"rules"`
	Source             string         `json:"source"` // registry|embedded
}

func packFromYAML(id, doc, source string) (*Pack, error) {
	v, err := ParseYAML(doc)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("pack %s: root not a map", id)
	}
	p := &Pack{Source: source, Provenance: map[string]any{}}
	p.ID = strOf(m["id"])
	p.Version = strOf(m["version"])
	p.EffectiveFrom = strOf(m["effective_from"])
	p.Status = strOf(m["status"])
	p.SubjectToRegazette, _ = m["subject_to_regazette"].(bool)
	if prov, ok := m["provenance"].(map[string]any); ok {
		p.Provenance = prov
	}
	if rules, ok := m["rules"].([]any); ok {
		for _, rv := range rules {
			rm, ok := rv.(map[string]any)
			if !ok {
				continue
			}
			r := Rule{ID: strOf(rm["id"])}
			if w, ok := rm["when"].(map[string]any); ok {
				r.When = w
			}
			if t, ok := rm["then"].(map[string]any); ok {
				r.Then = t
			}
			p.Rules = append(p.Rules, r)
		}
	}
	return p, nil
}

func strOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

// ---------- PackSet: rp-vat-* with registry API + embedded fallback ----------

type PackSet struct {
	cfg   Config
	mu    sync.RWMutex
	packs map[string]*Pack
}

func NewPackSet(cfg Config) *PackSet {
	return &PackSet{cfg: cfg, packs: map[string]*Pack{}}
}

var vatPackIDs = []string{
	"rp-vat-rates", "rp-vat-exempt-basket", "rp-vat-zerorated-basket",
	"rp-vat-attribution-mode", "rp-platform-collectors",
}

func (ps *PackSet) LoadPacks() {
	for _, id := range vatPackIDs {
		p, err := ps.fetchFromRegistry(id)
		if err != nil {
			p, err = packFromYAML(id, embeddedPacks[id], "embedded")
			if err != nil {
				logm("error", fmt.Sprintf("embedded pack %s failed: %v", id, err))
				continue
			}
		}
		ps.mu.Lock()
		ps.packs[id] = p
		ps.mu.Unlock()
		logm("info", fmt.Sprintf("loaded pack %s@%s (%s)", p.ID, p.Version, p.Source))
	}
}

func (ps *PackSet) fetchFromRegistry(id string) (*Pack, error) {
	if ps.cfg.RegistryURL == "" {
		return nil, fmt.Errorf("no registry configured")
	}
	resp, err := http.Get(ps.cfg.RegistryURL + "/v1/packs/" + id + "/latest")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("registry %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// registry may wrap the yaml in a JSON envelope {yaml: "..."} or serve raw
	if p, err := packFromYAML(id, string(body), "registry"); err == nil && p.ID != "" {
		return p, nil
	}
	var wrapper struct {
		YAML  string `json:"yaml"`
		Raw   string `json:"raw"`
		Content string `json:"content"`
	}
	if err := jsonUnmarshal(body, &wrapper); err == nil {
		doc := wrapper.YAML
		if doc == "" {
			doc = wrapper.Raw
		}
		if doc == "" {
			doc = wrapper.Content
		}
		if doc != "" {
			return packFromYAML(id, doc, "registry")
		}
	}
	return nil, fmt.Errorf("unparseable registry response for %s", id)
}

func (ps *PackSet) Get(id string) *Pack {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.packs[id]
}

func (ps *PackSet) Loaded() []Pack {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	out := make([]Pack, 0, len(ps.packs))
	for _, p := range ps.packs {
		out = append(out, *p)
	}
	return out
}

// VersionTag returns "rp-a@1.0.0,rp-b@1.0.0" for envelopes.
func (ps *PackSet) VersionTag() string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	parts := []string{}
	for _, id := range vatPackIDs {
		if p := ps.packs[id]; p != nil {
			parts = append(parts, p.ID+"@"+p.Version)
		}
	}
	return strings.Join(parts, ",")
}

// BasketFor classifies a line category into standard_75|zero_rated|exempt.
func (ps *PackSet) BasketFor(category string) string {
	cat := strings.ToLower(strings.TrimSpace(category))
	if cat == "" {
		return "standard_75"
	}
	if p := ps.Get("rp-vat-exempt-basket"); p != nil {
		for _, r := range p.Rules {
			if listMatch(r.Then["categories"], cat) {
				return "exempt"
			}
		}
	}
	if p := ps.Get("rp-vat-zerorated-basket"); p != nil {
		for _, r := range p.Rules {
			if listMatch(r.Then["categories"], cat) {
				return "zero_rated"
			}
		}
	}
	return "standard_75"
}

func listMatch(v any, cat string) bool {
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			if strings.ToLower(strOf(e)) == cat {
				return true
			}
		}
	case string:
		for _, e := range strings.Split(t, ",") {
			if strings.ToLower(strings.TrimSpace(e)) == cat {
				return true
			}
		}
	}
	return false
}

// StandardRateBPS returns the standard VAT rate in basis points (7.5% = 750).
func (ps *PackSet) StandardRateBPS() int64 {
	if p := ps.Get("rp-vat-rates"); p != nil {
		for _, r := range p.Rules {
			if r.ID == "vat.rate.standard" {
				if bps, ok := r.Then["rate_bps"].(int64); ok {
					return bps
				}
			}
		}
	}
	return 750
}

// AttributionConfig resolves the attribution mode + shares from rp-vat-attribution-mode.
type AttributionConfig struct {
	Mode            string `json:"mode"`
	FederalShareBPS int64  `json:"federal_share_bps"`
	StateShareBPS   int64  `json:"state_share_bps"`
	LGAShareBPS     int64  `json:"lga_share_bps"`
}

func (ps *PackSet) AttributionConfig(fallback string) AttributionConfig {
	cfg := AttributionConfig{Mode: fallback, FederalShareBPS: 1000, StateShareBPS: 5500, LGAShareBPS: 3500}
	if p := ps.Get("rp-vat-attribution-mode"); p != nil {
		for _, r := range p.Rules {
			if r.ID == "vat.attribution.mode" {
				if m := strOf(r.Then["mode"]); m != "" {
					cfg.Mode = m
				}
			}
			if r.ID == "vat.attribution.shares" {
				if v, ok := r.Then["federal_share_bps"].(int64); ok {
					cfg.FederalShareBPS = v
				}
				if v, ok := r.Then["state_share_bps"].(int64); ok {
					cfg.StateShareBPS = v
				}
				if v, ok := r.Then["lga_share_bps"].(int64); ok {
					cfg.LGAShareBPS = v
				}
			}
		}
	}
	return cfg
}

// IsPlatformCollector reports whether the merchant is a designated platform collector.
func (ps *PackSet) IsPlatformCollector(tin string) bool {
	if p := ps.Get("rp-platform-collectors"); p != nil {
		for _, r := range p.Rules {
			if listMatch(r.Then["tin_prefixes"], tinPrefix(tin)) {
				return true
			}
		}
	}
	return false
}

func tinPrefix(tin string) string {
	if len(tin) >= 4 {
		return tin[:4]
	}
	return tin
}
