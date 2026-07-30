package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// IngestRequest wraps a POS receipt with ingest options.
type IngestRequest struct {
	Receipt
	StoreAndForward bool `json:"store_and_forward,omitempty"` // offline terminal batch
	SpoolReason     string `json:"spool_reason,omitempty"`
}

func (s *Service) handleIngestReceipt(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req IngestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	if len(req.Lines) == 0 || req.ReceiptNo == "" {
		writeProblem(w, 422, "validation failed", "receipt_no and at least one line are required")
		return
	}
	// idempotency (hot cache: Redis or in-mem)
	idem := req.IdempotencyKey
	if idem == "" {
		idem = req.MerchantTIN + ":" + req.TerminalID + ":" + req.ReceiptNo
	}
	ok, err := s.cache.SetNX("idem:"+idem, "1", 24*time.Hour)
	if err == nil && !ok {
		if existing, found := s.store.GetReceipt("rcpt-" + idemHash(idem)); found {
			writeJSON(w, 200, map[string]any{"status": "duplicate", "receipt": existing})
			return
		}
	}

	if req.StoreAndForward {
		entry := SpoolEntry{ID: ULID(), QueuedAt: nowRFC3339(), Reason: orElse(req.SpoolReason, "offline-terminal"), Payload: json.RawMessage(mustJSON(req.Receipt))}
		if err := s.store.Spool(entry); err != nil {
			writeProblem(w, 500, "spool failed", err.Error())
			return
		}
		writeJSON(w, 202, map[string]any{"status": "spooled", "spool_id": entry.ID})
		return
	}

	rcpt, err := s.processReceipt(&req.Receipt, idem)
	if err != nil {
		writeProblem(w, 422, "processing failed", err.Error())
		return
	}
	s.bus.Publish(s.cfg.DataDir, "nrs.pos.receipts.v1", rcpt.TenantID, s.packs.VersionTag(), rcpt)
	w.Header().Set("X-Process-US", strconv.FormatInt(time.Since(start).Microseconds(), 10))
	writeJSON(w, 201, map[string]any{"status": "ingested", "receipt": rcpt})
}

func idemHash(k string) string {
	h := TINHash(k, "pos-idem")
	h = strings.NewReplacer("-", "", "_", "").Replace(h)
	if len(h) > 24 {
		h = h[:24]
	}
	return h
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// processReceipt runs wf-vat-normalise + wf-vat-attribution inline (hot path).
func (s *Service) processReceipt(rc *Receipt, idem string) (*Receipt, error) {
	if rc.ID == "" {
		rc.ID = "rcpt-" + idemHash(idem+ULID())
	}
	if rc.Currency == "" {
		rc.Currency = "NGN"
	}
	rc.RulePackVersion = s.packs.VersionTag()
	rc.MerchantTINHash = TINHash(rc.MerchantTIN, env("TIN_HMAC_KEY", "meridian-dev-tin-key"))
	if rc.CapturedAt == "" {
		rc.CapturedAt = nowRFC3339()
	}
	// normalise + classify baskets
	rate := s.packs.StandardRateBPS()
	rc.Baskets = map[string]int64{"standard_75": 0, "zero_rated": 0, "exempt": 0}
	var total, vat int64
	for _, ln := range rc.Lines {
		if ln.Qty <= 0 || ln.UnitPrice < 0 {
			return nil, fmt.Errorf("invalid line qty/price for sku %s", ln.SKU)
		}
		amount := ln.UnitPrice * ln.Qty / 1000 // qty in milli-units
		basket := s.packs.BasketFor(ln.Category)
		rc.Baskets[basket] += amount
		total += amount
		if basket == "standard_75" {
			vat += amount * rate / 10000
		}
	}
	rc.TotalKobo = total
	rc.VATKobo = vat
	// capture-time state/LGA attribution
	geo, err := s.geo.AttributePoint(rc.Lat, rc.Lon)
	if err != nil {
		return nil, fmt.Errorf("geo attribution: %w", err)
	}
	rc.State, rc.LGA = geo.State, geo.LGA
	// federal/state attribution (rp-vat-attribution-mode) with dual_shadow
	acfg := s.packs.AttributionConfig(s.cfg.AttributionMode)
	rc.Attribution = computeAttribution(vat, geo.State, acfg)
	rc.Status = "ingested"
	if err := s.store.PutReceipt(rc); err != nil {
		return nil, err
	}
	return rc, nil
}

func computeAttribution(vat int64, state string, acfg AttributionConfig) AttributionResult {
	res := AttributionResult{Mode: acfg.Mode, State: state}
	federal := vat * acfg.FederalShareBPS / 10000
	stateShare := vat * acfg.StateShareBPS / 10000
	switch acfg.Mode {
	case "federal":
		res.FederalKobo = vat
		res.StateKobo = 0
		res.ShadowFederal, res.ShadowState = federal, stateShare
	case "dual_shadow":
		// primary = state mode; shadow mirrors federal pool view
		res.FederalKobo = federal
		res.StateKobo = stateShare
		res.ShadowFederal, res.ShadowState = vat, 0
	default: // state
		res.FederalKobo = federal
		res.StateKobo = stateShare
	}
	return res
}

func (s *Service) handleListReceipts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	writeJSON(w, 200, map[string]any{"receipts": s.store.ListReceipts(q.Get("tenant_id"), q.Get("state"), limit)})
}

func (s *Service) handleGetReceipt(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.store.GetReceipt(r.PathValue("id"))
	if !ok {
		writeProblem(w, 404, "not found", "receipt not found")
		return
	}
	writeJSON(w, 200, rc)
}

func (s *Service) handleListSpool(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"spool": s.store.SpoolList()})
}

// handleSpoolDrain = wf-vat-spool-drain: replay spooled receipts through pipeline.
func (s *Service) handleSpoolDrain(w http.ResponseWriter, r *http.Request) {
	entries := s.store.SpoolList()
	drained, failed := 0, []string{}
	for _, e := range entries {
		var rc Receipt
		if err := json.Unmarshal(e.Payload, &rc); err != nil {
			failed = append(failed, e.ID+": unmarshal")
			continue
		}
		idem := rc.IdempotencyKey
		if idem == "" {
			idem = rc.MerchantTIN + ":" + rc.TerminalID + ":" + rc.ReceiptNo
		}
		out, err := s.processReceipt(&rc, idem)
		if err != nil {
			failed = append(failed, e.ID+": "+err.Error())
			continue
		}
		s.bus.Publish(s.cfg.DataDir, "nrs.pos.receipts.v1", out.TenantID, s.packs.VersionTag(), out)
		s.bus.Publish(s.cfg.DataDir, "nrs.pos.spool.drained.v1", out.TenantID, s.packs.VersionTag(), map[string]string{"spool_id": e.ID, "receipt_id": out.ID})
		s.store.SpoolRemove(e.ID)
		drained++
	}
	writeJSON(w, 200, map[string]any{"drained": drained, "failed": failed, "remaining": len(s.store.SpoolList())})
}

// handleSettlementRecon = wf-vat-settle-match: aggregate receipts over a
// period and post VAT remittance entries to ledger 300 (federal/state pools).
func (s *Service) handleSettlementRecon(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Period   string `json:"period"` // e.g. 2025-01
		TenantID string `json:"tenant_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	if req.Period == "" {
		writeProblem(w, 422, "validation failed", "period is required (YYYY-MM)")
		return
	}
	var vat, federal, state int64
	var count int
	for _, rc := range s.store.ListReceipts(req.TenantID, "", 0) {
		if strings.HasPrefix(rc.CapturedAt, req.Period) {
			vat += rc.VATKobo
			federal += rc.Attribution.FederalKobo
			state += rc.Attribution.StateKobo
			count++
		}
	}
	if count == 0 {
		writeProblem(w, 404, "no receipts", "no receipts in period "+req.Period)
		return
	}
	rec := &ReconRecord{ID: ULID(), Period: req.Period, TenantID: req.TenantID, Receipts: count,
		VATKobo: vat, FederalKobo: federal, StateKobo: state, LedgerMode: s.ledger.Mode(), PostedAt: nowRFC3339()}
	// settle: merchant clearing -> federal pool; merchant clearing -> state pool
	merchant := accountID(LedgerVATRemittance, NSVATMerchant)
	s.ledger.CreateAccounts([]LedgerAccount{{ID: merchant, Ledger: LedgerVATRemittance, Code: 4}})
	posted := []string{}
	if federal > 0 {
		tx, err := s.ledger.Transfer(LedgerTransfer{DebitAccountID: merchant, CreditAccountID: accountID(LedgerVATRemittance, NSVATFederalPool), AmountKobo: federal, Ledger: LedgerVATRemittance, Code: 5})
		if err != nil {
			writeProblem(w, 502, "ledger posting failed (federal)", err.Error())
			return
		}
		posted = append(posted, tx)
	}
	if state > 0 {
		tx, err := s.ledger.Transfer(LedgerTransfer{DebitAccountID: merchant, CreditAccountID: accountID(LedgerVATRemittance, NSVATStatePool), AmountKobo: state, Ledger: LedgerVATRemittance, Code: 5})
		if err != nil {
			writeProblem(w, 502, "ledger posting failed (state)", err.Error())
			return
		}
		posted = append(posted, tx)
	}
	rec.LedgerTransfer = strings.Join(posted, ",")
	s.store.AddRecon(rec)
	s.bus.Publish(s.cfg.DataDir, "nrs.pos.settlement.recon.v1", req.TenantID, s.packs.VersionTag(), rec)
	writeJSON(w, 201, rec)
}

func (s *Service) handleListRecon(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"recon": s.store.Recons()})
}

// handleVariance = wf-vat-variance: detect receipts whose expected VAT
// (from baskets) diverges from terminal-declared totals, plus attribution
// reconciliation drift beyond 1 kobo rounding tolerance.
func (s *Service) handleVariance(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tenant := q.Get("tenant_id")
	type varianceRow struct {
		ReceiptID   string `json:"receipt_id"`
		Kind        string `json:"kind"`
		Expected    int64  `json:"expected_kobo"`
		Actual      int64  `json:"actual_kobo"`
		Delta       int64  `json:"delta_kobo"`
		Explanation string `json:"explanation"`
	}
	rows := []varianceRow{}
	rate := s.packs.StandardRateBPS()
	for _, rc := range s.store.ListReceipts(tenant, "", 0) {
		recompute := rc.Baskets["standard_75"] * rate / 10000
		if delta := rc.VATKobo - recompute; delta < -1 || delta > 1 {
			rows = append(rows, varianceRow{rc.ID, "vat-recompute-drift", recompute, rc.VATKobo, delta,
				"computed VAT diverges from basket recompute beyond rounding tolerance"})
		}
		attrSum := rc.Attribution.FederalKobo + rc.Attribution.StateKobo
		if rc.Attribution.Mode != "federal" {
			if delta := rc.VATKobo - attrSum; delta < -1 || delta > 1 {
				rows = append(rows, varianceRow{rc.ID, "attribution-sum-mismatch", rc.VATKobo, attrSum, delta,
					"federal+state attribution does not reconcile to VAT charged"})
			}
		}
	}
	writeJSON(w, 200, map[string]any{"variance_count": len(rows), "variances": rows})
}

// handleB2CReport = wf-vat-b2c-report: near-real-time B2C transaction report.
func (s *Service) handleB2CReport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TenantID string `json:"tenant_id"`
		Period   string `json:"period"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	type row struct {
		ReceiptID  string `json:"receipt_id"`
		CapturedAt string `json:"captured_at"`
		TotalKobo  int64  `json:"total_kobo"`
		VATKobo    int64  `json:"vat_kobo"`
		State      string `json:"state"`
		TINHash    string `json:"merchant_tin_hash"`
	}
	rows := []row{}
	var totVAT, totSales int64
	for _, rc := range s.store.ListReceipts(req.TenantID, "", 0) {
		if req.Period != "" && !strings.HasPrefix(rc.CapturedAt, req.Period) {
			continue
		}
		rows = append(rows, row{rc.ID, rc.CapturedAt, rc.TotalKobo, rc.VATKobo, rc.State, rc.MerchantTINHash})
		totVAT += rc.VATKobo
		totSales += rc.TotalKobo
	}
	report := map[string]any{
		"report_id": ULID(), "tenant_id": req.TenantID, "period": req.Period,
		"generated_at": nowRFC3339(), "transactions": len(rows),
		"total_sales_kobo": totSales, "total_vat_kobo": totVAT, "rows": rows,
	}
	s.bus.Publish(s.cfg.DataDir, "nrs.pos.b2c.report.v1", req.TenantID, s.packs.VersionTag(), report)
	writeJSON(w, 201, report)
}

// handleCertRun = wf-vat-cert-run: certification pass over stored receipts
// verifying basket classification against pack definitions and geo
// attribution consistency. Produces a signed-style report (sha256 digest).
func (s *Service) handleCertRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TenantID string `json:"tenant_id"`
		Sample   int    `json:"sample"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	if req.Sample <= 0 {
		req.Sample = 50
	}
	receipts := s.store.ListReceipts(req.TenantID, "", req.Sample)
	checks, passed := 0, 0
	failures := []map[string]any{}
	for _, rc := range receipts {
		for _, ln := range rc.Lines {
			checks++
			want := s.packs.BasketFor(ln.Category)
			amount := ln.UnitPrice * ln.Qty / 1000
			ok := true
			switch want {
			case "exempt", "zero_rated":
				ok = rc.Baskets[want] >= amount
			default:
				ok = rc.Baskets["standard_75"] >= amount
			}
			if ok {
				passed++
			} else {
				failures = append(failures, map[string]any{"receipt": rc.ID, "sku": ln.SKU, "basket": want})
			}
		}
		checks++
		if _, err := (EmbeddedGeo{}).AttributePoint(rc.Lat, rc.Lon); err == nil {
			passed++
		} else {
			failures = append(failures, map[string]any{"receipt": rc.ID, "geo": err.Error()})
		}
	}
	digest := TINHash(fmt.Sprintf("%s|%d|%d|%d", req.TenantID, checks, passed, time.Now().Unix()), "cert")
	report := map[string]any{
		"cert_id": "cert-" + digest[:16], "tenant_id": req.TenantID, "run_at": nowRFC3339(),
		"checks": checks, "passed": passed, "failed": checks - passed,
		"verdict": map[bool]string{true: "PASS", false: "FAIL"}[len(failures) == 0],
		"failures": failures, "rule_pack_version": s.packs.VersionTag(),
		"digest": digest,
	}
	s.bus.Publish(s.cfg.DataDir, "nrs.pos.cert.run.v1", req.TenantID, s.packs.VersionTag(), report)
	writeJSON(w, 201, report)
}

func (s *Service) handleAttributionMode(w http.ResponseWriter, r *http.Request) {
	cfg := s.packs.AttributionConfig(s.cfg.AttributionMode)
	writeJSON(w, 200, map[string]any{
		"mode": cfg.Mode, "federal_share_bps": cfg.FederalShareBPS,
		"state_share_bps": cfg.StateShareBPS, "lga_share_bps": cfg.LGAShareBPS,
		"source": "rp-vat-attribution-mode", "rule_pack_version": s.packs.VersionTag(),
	})
}

func (s *Service) handlePacks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"packs": s.packs.Loaded()})
}

func (s *Service) handleEvents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"events": s.bus.Recent(50)})
}
