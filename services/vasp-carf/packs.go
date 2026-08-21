package main

import (
	"encoding/json"
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
	Source             string         `json:"source"`
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

// ---------- pack loading: rp-registry API + embedded fallback ----------

type PackSet struct {
	registryURL string
	mu          sync.RWMutex
	packs       map[string]*Pack
}

var vaspPackIDs = []string{"rp-carf-schema", "rp-nta-vasp-duties", "rp-nta-digital-assets", "rp-asset-taxonomy"}

func NewPackSet(registryURL string) *PackSet {
	return &PackSet{registryURL: registryURL, packs: map[string]*Pack{}}
}

func (ps *PackSet) LoadAll() {
	for _, id := range vaspPackIDs {
		p, err := ps.fetch(id)
		if err != nil {
			p, err = packFromYAML(id, embeddedVaspPacks[id], "embedded")
			if err != nil {
				logm("error", fmt.Sprintf("embedded pack %s: %v", id, err))
				continue
			}
		}
		ps.mu.Lock()
		ps.packs[id] = p
		ps.mu.Unlock()
		logm("info", fmt.Sprintf("loaded pack %s@%s (%s)", p.ID, p.Version, p.Source))
	}
}

func (ps *PackSet) fetch(id string) (*Pack, error) {
	if ps.registryURL == "" {
		return nil, fmt.Errorf("no registry")
	}
	resp, err := registryHTTPClient.Get(ps.registryURL + "/v1/packs/" + id + "/latest")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("registry %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if p, err := packFromYAML(id, string(body), "registry"); err == nil && p.ID != "" {
		return p, nil
	}
	var wrapper struct {
		YAML, Raw, Content string
	}
	if json.Unmarshal(body, &wrapper) == nil {
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
	return nil, fmt.Errorf("unparseable registry response")
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

func (ps *PackSet) VersionTag() string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	parts := []string{}
	for _, id := range vaspPackIDs {
		if p := ps.packs[id]; p != nil {
			parts = append(parts, p.ID+"@"+p.Version)
		}
	}
	return strings.Join(parts, ",")
}

// DutiesFor evaluates rp-nta-vasp-duties rules against a trade context.
func (ps *PackSet) DutiesFor(ctx map[string]any) []map[string]any {
	out := []map[string]any{}
	p := ps.Get("rp-nta-vasp-duties")
	if p == nil {
		return out
	}
	for _, r := range p.Rules {
		if whenMatches(r.When, ctx) {
			out = append(out, map[string]any{
				"rule_id": r.ID, "pack": p.ID + "@" + p.Version,
				"duty": r.Then, "narrate": strOf(r.Then["narrate"]),
			})
		}
	}
	return out
}

func whenMatches(when map[string]any, ctx map[string]any) bool {
	for k, v := range when {
		cv, ok := ctx[k]
		if !ok {
			return false
		}
		if strings.ToLower(strOf(cv)) != strings.ToLower(strOf(v)) {
			return false
		}
	}
	return true
}

// CARFRequiredFields returns rp-carf-schema required field rules.
func (ps *PackSet) CARFRequiredFields() []string {
	p := ps.Get("rp-carf-schema")
	if p == nil {
		return []string{"message_ref_id", "timestamp", "reporting_vasp", "reportable_user", "transactions"}
	}
	for _, r := range p.Rules {
		if r.ID == "carf.message.required" {
			if fields, ok := r.Then["fields"].([]any); ok {
				out := []string{}
				for _, f := range fields {
					out = append(out, strOf(f))
				}
				return out
			}
		}
	}
	return []string{"message_ref_id", "timestamp", "reporting_vasp", "reportable_user", "transactions"}
}

// ---------- embedded fallback packs (pinned core contracts v1) ----------

var embeddedVaspPacks = map[string]string{
	"rp-carf-schema": `id: rp-carf-schema
version: 1.0.0
effective_from: 2026-01-01
effective_to: null
status: published
subject_to_regazette: true
provenance:
  as_passed: "OECD Crypto-Asset Reporting Framework (CARF) 2023 message structure"
  as_gazetted: null
  source_citation: "OECD CARF XML Schema v1.1; NTA 2025 VASP reporting provisions"
rules:
  - id: carf.message.required
    when: {}
    then: { fields: [message_ref_id, timestamp, reporting_vasp, reportable_user, transactions], narrate: "CARF message mandatory elements" }
  - id: carf.message.types
    when: {}
    then: { types: [RC701, RC702, RC703, RC704], narrate: "RC701 exchange fiat-crypto, RC702 exchange crypto-crypto, RC703 transfer, RC704 retail payment" }
  - id: carf.correction.protocol
    when: {}
    then: { doc_type_indic: [OECD1, OECD2, OECD3], narrate: "OECD1 new, OECD2 correction, OECD3 deletion" }
`,
	"rp-nta-vasp-duties": `id: rp-nta-vasp-duties
version: 1.0.0
effective_from: 2025-01-01
effective_to: null
status: published
subject_to_regazette: true
provenance:
  as_passed: "Nigeria Tax Act 2025: digital asset gains chargeable; VASP record-keeping and reporting duties"
  as_gazetted: null
  source_citation: "Nigeria Tax Act 2025 (digital assets); SEC VASP Rules 2022"
rules:
  - id: vasp.duty.recordkeeping
    when: { actor: vasp }
    then: { duty: retain_records, years: 6, narrate: "VASP must retain transaction records 6 years" }
  - id: vasp.duty.reporting
    when: { actor: vasp }
    then: { duty: carf_report, frequency: annual, narrate: "Annual CARF report of user transactions" }
  - id: vasp.duty.gains
    when: { actor: user }
    then: { duty: declare_gains, ring_fenced: true, narrate: "Digital asset gains taxable; losses ring-fenced to digital assets" }
  - id: vasp.duty.large_tx
    when: { actor: vasp }
    then: { duty: enhanced_due_diligence, threshold_kobo: 500000000, narrate: "Enhanced due diligence above NGN 5m single transaction" }
`,
	"rp-nta-digital-assets": `id: rp-nta-digital-assets
version: 1.0.0
effective_from: 2025-01-01
effective_to: null
status: published
subject_to_regazette: true
provenance:
  as_passed: "NTA 2025: digital assets chargeable assets; loss ring-fencing"
  as_gazetted: null
  source_citation: "Nigeria Tax Act 2025 (chargeable assets: digital assets)"
rules:
  - id: vasp.ringfence
    when: { asset_class: digital_asset }
    then: { ring_fenced: true, loss_offset_scope: digital_asset_only, carry_forward: true, narrate: "Digital asset losses offset digital asset gains only" }
  - id: vasp.basis.methods
    when: {}
    then: { methods: [fifo, wac], default: fifo, narrate: "Permitted cost-basis methods" }
  - id: vasp.disposal.events
    when: {}
    then: { events: [sell, exchange, transfer_out, spend], narrate: "Disposal events triggering gain/loss" }
`,
	"rp-asset-taxonomy": `id: rp-asset-taxonomy
version: 1.0.0
effective_from: 2025-01-01
effective_to: null
status: published
subject_to_regazette: true
provenance:
  as_passed: "CARF taxonomy: crypto-assets by class"
  as_gazetted: null
  source_citation: "OECD CARF crypto-asset taxonomy; NTA 2025"
rules:
  - id: asset.class.crypto
    when: {}
    then: { assets: [BTC, ETH, LTC, XRP, SOL], class: crypto_asset, narrate: "Native crypto-assets" }
  - id: asset.class.stablecoin
    when: {}
    then: { assets: [USDT, USDC, BUSD, ENGN], class: stablecoin, narrate: "Stablecoins (incl. eNaira-wrapped)" }
  - id: asset.class.token
    when: {}
    then: { assets: [UNI, AAVE, MKR], class: utility_token, narrate: "Utility/governance tokens" }
`,
}
