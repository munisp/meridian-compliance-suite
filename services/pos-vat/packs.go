package main

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// registryHTTPClient bounds pack-registry fetches (QA-29): the default
// http.Client has no timeout and could hang a request worker indefinitely.
var registryHTTPClient = &http.Client{Timeout: 10 * time.Second}

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
		if ln.indent != indent || (!strings.HasPrefix(ln.content, "- ") && ln.content != "-") {
			break
		}
		rest := strings.TrimPrefix(ln.content, "- ")
		if rest == ln.content {
			rest = ""
		}
		if rest == "" {
			i++
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
		// inline scalar or start of a nested map on the same line ("- id: x")
		if k, v, ok := splitKV(rest); ok && !strings.HasPrefix(v, "{") {
			// nested map beginning with an inline pair
			m := map[string]any{}
			sv, err := parseScalar(v)
			if err != nil {
				return nil, i, err
			}
			m[k] = sv
			i++
			for i < len(lines) && lines[i].indent > indent {
				sub, ni, err := parseMap(lines, i, lines[i].indent)
				if err != nil {
					return nil, ni, err
				}
				for kk, vv := range sub.(map[string]any) {
					m[kk] = vv
				}
				i = ni
				if i < len(lines) && lines[i].indent > indent {
					continue
				}
				break
			}
			out = append(out, m)
			continue
		}
		v, err := parseScalar(rest)
		if err != nil {
			return nil, i, err
		}
		out = append(out, v)
		i++
	}
	return out, i, nil
}

func splitKV(s string) (string, string, bool) {
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
		case ':':
			if !inS && !inD && (i+1 == len(s) || s[i+1] == ' ') {
				return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), true
			}
		}
	}
	return "", "", false
}

func parseScalar(s string) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" || s == "~" {
		return nil, nil
	}
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		m := map[string]any{}
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return m, nil
		}
		for _, part := range splitFlow(inner) {
			k, v, ok := splitKV(part)
			if !ok {
				return nil, fmt.Errorf("bad flow pair %q", part)
			}
			sv, err := parseScalar(v)
			if err != nil {
				return nil, err
			}
			m[k] = sv
		}
		return m, nil
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		var out []any
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return out, nil
		}
		for _, part := range splitFlow(inner) {
			v, err := parseScalar(part)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1], nil
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	}
	if s == "true" {
		return true, nil
	}
	if s == "false" {
		return false, nil
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f, nil
	}
	return s, nil
}

func splitFlow(s string) []string {
	var out []string
	depth := 0
	inS, inD := false, false
	start := 0
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
		case '{', '[':
			if !inS && !inD {
				depth++
			}
		case '}', ']':
			if !inS && !inD {
				depth--
			}
		case ',':
			if !inS && !inD && depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(s[start:]))
	return out
}

// ---------- rp-* pack loading: registry-first with embedded fallback (SPEC §1.4) ----------

type RulePack struct {
	ID                    string
	Version               string
	EffectiveFrom         string
	EffectiveTo           string
	Status                string // draft|published|gazetted|archived
	SubjectToRegazette    bool
	Provenance            map[string]any
	Signed                map[string]any
	Rules                 []Rule
}

type Rule struct {
	ID   string
	When map[string]any
	Then map[string]any
}

func packFromYAML(id string, raw []byte) (*RulePack, error) {
	doc, err := ParseYAML(string(raw))
	if err != nil {
		return nil, err
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("pack root not a map")
	}
	p := &RulePack{ID: id}
	p.Version = strOf(m["version"])
	p.EffectiveFrom = strOf(m["effective_from"])
	p.EffectiveTo = strOf(m["effective_to"])
	p.Status = strOf(m["status"])
	p.SubjectToRegazette, _ = m["subject_to_regazette"].(bool)
	p.Provenance, _ = m["provenance"].(map[string]any)
	p.Signed, _ = m["signed"].(map[string]any)
	rules, _ := m["rules"].([]any)
	for _, rv := range rules {
		rm, ok := rv.(map[string]any)
		if !ok {
			continue
		}
		r := Rule{ID: strOf(rm["id"])}
		r.When, _ = rm["when"].(map[string]any)
		r.Then, _ = rm["then"].(map[string]any)
		p.Rules = append(p.Rules, r)
	}
	if p.Version == "" {
		return nil, fmt.Errorf("pack %s missing version", id)
	}
	return p, nil
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// PackSet holds the active packs for a service.
type PackSet struct {
	mu    sync.RWMutex
	packs map[string]*RulePack
	cfg   Config
}

func NewPackSet(cfg Config) *PackSet {
	return &PackSet{packs: map[string]*RulePack{}, cfg: cfg}
}

// LoadPacks resolves every pack: registry (RULE_PACK_REGISTRY_URL) first,
// embedded fallback. Fail-closed in prod: no registry, no pack.
func (ps *PackSet) LoadPacks() {
	for _, id := range packOrder {
		if p := ps.fetchFromRegistry(id); p != nil {
			ps.mu.Lock()
			ps.packs[id] = p
			ps.mu.Unlock()
			logm("info", "pack "+id+" loaded from registry v"+p.Version)
			continue
		}
		if raw, ok := embeddedPacks[id]; ok {
			p, err := packFromYAML(id, []byte(raw))
			if err == nil {
				ps.mu.Lock()
				ps.packs[id] = p
				ps.mu.Unlock()
				logm("info", "pack "+id+" loaded from embedded fallback v"+p.Version)
				continue
			}
			logm("error", "embedded pack "+id+" failed: "+err.Error())
		}
		if ps.cfg.AuthMode == "prod" {
			logm("error", "pack "+id+" unavailable in prod (fail-closed)")
		}
	}
}

func (ps *PackSet) fetchFromRegistry(id string) *RulePack {
	base := ps.cfg.RegistryURL
	if base == "" {
		return nil
	}
	resp, err := registryHTTPClient.Get(strings.TrimRight(base, "/") + "/packs/" + id + "/active")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var payload struct {
		ID      string `json:"id"`
		Content string `json:"content"` // raw YAML (registry is the source of truth)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := jsonUnmarshal(body, &payload); err != nil || payload.Content == "" {
		return nil
	}
	p, err := packFromYAML(id, []byte(payload.Content))
	if err != nil {
		return nil
	}
	return p
}

// Active returns the active pack for id (nil if not loaded).
func (ps *PackSet) Get(id string) *RulePack {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.packs[id]
}

func (ps *PackSet) Loaded() []string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	out := []string{}
	for id := range ps.packs {
		out = append(out, id)
	}
	return out
}

// VersionString renders the loaded packs for event metadata.
func (ps *PackSet) VersionString() string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	parts := []string{}
	for _, id := range packOrder {
		if p := ps.packs[id]; p != nil {
			parts = append(parts, id+"@"+p.Version)
		}
	}
	return strings.Join(parts, ",")
}

// evaluateMatches collects then-maps of all rules whose when-clauses match.
func evaluateMatches(p *RulePack, ctx map[string]any) []map[string]any {
	var out []map[string]any
	for _, r := range p.Rules {
		if whenMatches(r.When, ctx) {
			out = append(out, r.Then)
		}
	}
	return out
}

func whenMatches(when map[string]any, ctx map[string]any) bool {
	for k, want := range when {
		base, op := k, "eq"
		for _, suffix := range []string{"__gt", "__gte", "__lt", "__lte", "__ne", "__in", "__prefix"} {
			if strings.HasSuffix(k, suffix) {
				base = strings.TrimSuffix(k, suffix)
				op = strings.TrimPrefix(suffix, "__")
				break
			}
		}
		got := ctx[base]
		switch op {
		case "eq":
			if !scalarEq(got, want) {
				return false
			}
		case "ne":
			if scalarEq(got, want) {
				return false
			}
		case "gt", "gte", "lt", "lte":
			if !numCmp(got, want, op) {
				return false
			}
		case "in":
			if !listMatch(want, strOf(got)) {
				return false
			}
		case "prefix":
			if !strings.HasPrefix(strOf(got), strOf(want)) {
				return false
			}
		}
	}
	return true
}

func scalarEq(a, b any) bool {
	if ai, ok := a.(int64); ok {
		switch bv := b.(type) {
		case int64:
			return ai == bv
		case float64:
			return float64(ai) == bv
		}
	}
	if af, ok := a.(float64); ok {
		switch bv := b.(type) {
		case int64:
			return af == float64(bv)
		case float64:
			return af == bv
		}
	}
	return strOf(a) == strOf(b) || a == b
}

func numCmp(a, b any, op string) bool {
	var af, bf float64
	switch v := a.(type) {
	case int64:
		af = float64(v)
	case float64:
		af = v
	default:
		return false
	}
	switch v := b.(type) {
	case int64:
		bf = float64(v)
	case float64:
		bf = v
	default:
		return false
	}
	switch op {
	case "gt":
		return af > bf
	case "gte":
		return af >= bf
	case "lt":
		return af < bf
	case "lte":
		return af <= bf
	}
	return false
}

func listMatch(v any, cat string) bool {
	switch l := v.(type) {
	case []any:
		for _, e := range l {
			if strings.ToLower(strOf(e)) == cat {
				return true
			}
		}
	case string:
		for _, e := range strings.Split(l, ",") {
			if strings.ToLower(strings.TrimSpace(e)) == cat {
				return true
			}
		}
	}
	return false
}

// ---------- rp-vat-nigeria helpers ----------

// BasketForSKU maps a SKU category hint to a VAT basket using rp-vat-nigeria rules.
func (ps *PackSet) BasketForSKU(category string) string {
	if p := ps.Get("rp-vat-nigeria"); p != nil {
		cat := strings.ToLower(category)
		for _, r := range p.Rules {
			if listMatch(r.Then["categories"], cat) {
				return strOf(r.Then["basket"])
			}
		}
	}
	switch strings.ToLower(category) {
	case "food", "staples", "unprocessed food":
		return "food_staples"
	case "medicine", "pharma", "medical":
		return "medicines"
	case "book", "books", "education":
		return "education"
	case "export":
		return "exports"
	default:
		return "general"
	}
}

// VatRateBps resolves the VAT rate for a basket from rp-vat-nigeria (default 750).
func (ps *PackSet) VatRateBps(basket string) int64 {
	if p := ps.Get("rp-vat-nigeria"); p != nil {
		for _, r := range p.Rules {
			if strOf(r.Then["basket"]) == basket {
				if rate, ok := r.Then["rate_bps"].(int64); ok {
					return rate
				}
			}
		}
	}
	switch basket {
	case "food_staples", "medicines", "education", "exports":
		return 0
	default:
		return 750
	}
}

// IsPlatformCollector reports whether the merchant is a designated platform
// collector. listMatch lowercases the pack entries, so the TIN prefix is
// lowered here too (audit R4 S1a#8: the case mismatch meant this predicate
// could never fire — one reason the regime was dead code).
func (ps *PackSet) IsPlatformCollector(tin string) bool {
	if p := ps.Get("rp-platform-collectors"); p != nil {
		for _, r := range p.Rules {
			if listMatch(r.Then["tin_prefixes"], strings.ToLower(tinPrefix(tin))) {
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

// ---------- R4 build repair: compatibility surface restored after the ----------
// Pack->RulePack refactor in #58 (handlers.go/citations.go still reference it).

// Pack is the pre-#58 name for a rule pack; kept as an alias so existing
// call-sites (citations.go) keep working.
type Pack = RulePack

// packOrder is the deterministic load/render order of the embedded packs
// (pre-#58 this was vatPackIDs; recovered from 539dd8e^:packs.go).
var packOrder = []string{
	"rp-vat-rates", "rp-vat-exempt-basket", "rp-vat-zerorated-basket",
	"rp-vat-attribution-mode", "rp-platform-collectors",
}

// VersionTag returns "rp-a@1.0.0,rp-b@1.0.0" for envelopes (= VersionString).
func (ps *PackSet) VersionTag() string { return ps.VersionString() }

// BasketFor classifies a line category into standard_75|zero_rated|exempt
// using the rp-vat-exempt-basket / rp-vat-zerorated-basket packs.
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

// StandardRateBPS returns the standard VAT rate in basis points (7.5% = 750)
// from the rp-vat-rates pack (rule vat.rate.standard).
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

// AttributionConfig resolves the attribution mode + shares from
// rp-vat-attribution-mode (defaults 1000/5500/3500 BPS).
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
