package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/munisp/meridian-compliance-suite/packages/authx"
	"github.com/munisp/meridian-compliance-suite/packages/httpx"
)

type Config struct {
	Port        string
	AuthMode    string
	JWTSecret   string
	RegistryURL string
	RegWatchURL string
	DataDir     string
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type Service struct {
	cfg    Config
	engine *Engine
	packs  *PackSet
	gates  *GateChecker
	carf   *CARFStore
	kc     *authx.KeycloakVerifier // non-nil when AUTH_MODE=keycloak (SPEC §1.3)
}

func main() {
	cfg := Config{
		Port:        env("PORT", "8116"),
		AuthMode:    env("AUTH_MODE", "dev"),
		JWTSecret:   env("MERIDIAN_DEV_JWT_SECRET", "meridian-dev-secret"),
		RegistryURL: env("RP_REGISTRY_URL", ""),
		RegWatchURL: env("REG_WATCH_URL", ""),
		DataDir:     env("DATA_DIR", "./data"),
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("datadir: %v", err)
	}
	svc := &Service{
		cfg: cfg, engine: NewEngine(cfg.DataDir), packs: NewPackSet(cfg.RegistryURL),
		gates: NewGateChecker(cfg.RegWatchURL, cfg.DataDir), carf: NewCARFStore(cfg.DataDir),
	}
	// SPEC §1.3: AUTH_MODE=keycloak selects RS256/JWKS verification.
	// FAIL CLOSED: a keycloak deployment missing OIDC config refuses to boot
	// rather than silently run the forgeable dev HS256 auth.
	if cfg.AuthMode == "keycloak" {
		v := authx.KeycloakVerifierFromEnv()
		if v == nil {
			log.Fatal("AUTH_MODE=keycloak but KEYCLOAK_ISSUER is unset; refusing to start (no dev fallback)")
		}
		svc.kc = v
		log.Printf("profile=prod component=auth (keycloak issuer=%s)", v.Issuer)
	} else {
		log.Printf("profile=dev component=auth")
	}
	svc.packs.LoadAll()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "service": "vasp-carf", "version": "1.0.0"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"status": "ready", "packs": len(svc.packs.Loaded())})
	})
	mux.HandleFunc("POST /v1/trades", svc.auth(svc.handleIngestTrade))
	mux.HandleFunc("GET /v1/trades", svc.auth(svc.handleListTrades))
	mux.HandleFunc("POST /v1/transfers", svc.auth(svc.handleIngestTransfer))
	mux.HandleFunc("GET /v1/transfers", svc.auth(svc.handleListTransfers))
	mux.HandleFunc("GET /v1/costbasis/{asset}", svc.auth(svc.handleCostBasis))
	mux.HandleFunc("POST /v1/fmv/snapshots", svc.auth(svc.handleAddFMV))
	mux.HandleFunc("GET /v1/fmv/{asset}", svc.auth(svc.handleGetFMV))
	mux.HandleFunc("POST /v1/ringfence/compute", svc.auth(svc.handleRingFence))
	mux.HandleFunc("GET /v1/gains", svc.auth(svc.handleGains))
	mux.HandleFunc("GET /v1/ledger", svc.auth(svc.handleGains)) // accounting ledger alias
	mux.HandleFunc("POST /v1/carf/build", svc.auth(svc.handleCARFBuild))
	mux.HandleFunc("GET /v1/carf/messages", svc.auth(svc.handleCARFList))
	mux.HandleFunc("GET /v1/carf/messages/{id}", svc.auth(svc.handleCARFGet))
	mux.HandleFunc("GET /v1/carf/messages/{id}/xml", svc.auth(svc.handleCARFXML))
	mux.HandleFunc("POST /v1/carf/messages/{id}/correct", svc.auth(svc.handleCARFCorrect))
	mux.HandleFunc("POST /v1/carf/transmit", svc.auth(svc.handleCARFTransmit))
	mux.HandleFunc("GET /v1/gates", svc.auth(svc.handleGates))
	mux.HandleFunc("POST /v1/gates/{id}/flip", svc.auth(svc.handleGateFlip)) // dev board-authorized
	mux.HandleFunc("POST /v1/duties/evaluate", svc.auth(svc.handleDuties))
	mux.HandleFunc("GET /v1/packs", svc.auth(svc.handlePacks))

	// F-5: graceful shutdown on SIGTERM/SIGINT + full server timeouts.
	srv := httpx.NewServer(":"+cfg.Port, svc.recover(svc.logging(mux)))
	log.Printf("vasp-carf listening on :%s (auth=%s registry=%s regwatch=%s)",
		cfg.Port, cfg.AuthMode, orDef(cfg.RegistryURL, "embedded"), orDef(cfg.RegWatchURL, "local-gate-file"))
	log.Fatal(httpx.Serve(srv))
}

func orDef(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

type ctxKey string

const claimsCtxKey ctxKey = "claims"

// claimsOf returns the authenticated principal (nil when unauthenticated).
func claimsOf(r *http.Request) *Claims {
	if c, ok := r.Context().Value(claimsCtxKey).(*Claims); ok {
		return c
	}
	return nil
}

func hasRole(c *Claims, role string) bool {
	if c == nil {
		return false
	}
	for _, r := range c.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// auth implements SPEC §1.3: keycloak mode verifies RS256 against the realm
// JWKS (fail-closed; no X-Dev-Role); dev mode accepts HS256 Bearer tokens and
// an allowlisted X-Dev-Role header.
func (s *Service) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if strings.HasPrefix(h, "Bearer ") {
			tok := strings.TrimPrefix(h, "Bearer ")
			if s.kc != nil {
				c, err := s.kc.Verify(tok)
				if err != nil {
					writeProblem(w, 401, "unauthorized", err.Error())
					return
				}
				claims := &Claims{Sub: c.Sub, Roles: c.Roles, TenantID: c.TenantID, Exp: c.Exp}
				next(w, r.WithContext(context.WithValue(r.Context(), claimsCtxKey, claims)))
				return
			}
			c, err := verifyHS256(tok, s.cfg.JWTSecret)
			if err != nil {
				writeProblem(w, 401, "unauthorized", err.Error())
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), claimsCtxKey, c)))
			return
		}
		if s.kc == nil && s.cfg.AuthMode == "dev" {
			switch role := r.Header.Get("X-Dev-Role"); role {
			case "admin", "operator", "auditor":
				c := &Claims{Sub: "dev-" + role, Roles: []string{role}, TenantID: r.Header.Get("X-Tenant-ID")}
				next(w, r.WithContext(context.WithValue(r.Context(), claimsCtxKey, c)))
				return
			}
		}
		writeProblem(w, 401, "unauthorized", "Bearer JWT or X-Dev-Role (dev mode) required")
	}
}

func (s *Service) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			logm("info", r.Method+" "+r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Service) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				writeProblem(w, 500, "internal error", "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------- handlers ----------

func (s *Service) handleIngestTrade(w http.ResponseWriter, r *http.Request) {
	var t Trade
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&t); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	if t.UserHash == "" {
		writeProblem(w, 422, "validation failed", "user_hash (pseudonymised) is required")
		return
	}
	method := r.URL.Query().Get("method")
	gl, err := s.engine.IngestTrade(&t, method)
	if err != nil {
		writeProblem(w, 422, "trade rejected", err.Error())
		return
	}
	code := 201
	out := map[string]any{"status": "ingested", "trade_id": t.ID}
	if gl != nil {
		out["gain_loss"] = gl
	}
	writeJSON(w, code, out)
}

func (s *Service) handleListTrades(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"trades": s.engine.Trades(r.URL.Query().Get("tenant_id"))})
}

func (s *Service) handleIngestTransfer(w http.ResponseWriter, r *http.Request) {
	var tr Transfer
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&tr); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	gl, err := s.engine.IngestTransfer(&tr)
	if err != nil {
		writeProblem(w, 422, "transfer rejected", err.Error())
		return
	}
	out := map[string]any{"status": "ingested", "transfer_id": tr.ID, "fmv_kobo": tr.FMVKobo}
	if gl != nil {
		out["gain_loss"] = gl
	}
	writeJSON(w, 201, out)
}

func (s *Service) handleListTransfers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"transfers": s.engine.Transfers(r.URL.Query().Get("tenant_id"))})
}

func (s *Service) handleCostBasis(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	method := q.Get("method")
	if method == "" {
		method = "fifo"
	}
	writeJSON(w, 200, s.engine.CostBasis(q.Get("tenant_id"), q.Get("user_hash"), r.PathValue("asset"), method))
}

func (s *Service) handleAddFMV(w http.ResponseWriter, r *http.Request) {
	var snap FMVSnapshot
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&snap); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	if snap.Asset == "" || snap.PriceKobo <= 0 {
		writeProblem(w, 422, "validation failed", "asset and positive price_kobo required")
		return
	}
	s.engine.AddFMV(snap)
	writeJSON(w, 201, map[string]string{"status": "cached", "asset": snap.Asset})
}

func (s *Service) handleGetFMV(w http.ResponseWriter, r *http.Request) {
	if snap, ok := s.engine.LatestFMV(r.PathValue("asset")); ok {
		writeJSON(w, 200, snap)
		return
	}
	writeProblem(w, 404, "no snapshot", "no FMV snapshot cached for asset")
}

func (s *Service) handleRingFence(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TenantID string `json:"tenant_id"`
		UserHash string `json:"user_hash"`
		Period   string `json:"period"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	writeJSON(w, 200, s.engine.RingFence(req.TenantID, req.UserHash, req.Period, s.packs.Get("rp-nta-digital-assets")))
}

func (s *Service) handleGains(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"gain_loss_ledger": s.engine.Ledger(r.URL.Query().Get("tenant_id")), "note": "accounting ledger, not payments"})
}

func (s *Service) handleCARFBuild(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TenantID string `json:"tenant_id"`
		Period   string `json:"period"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	rec, err := BuildCARFMessage(s, req.TenantID, req.Period, "OECD1", "", "")
	if err != nil {
		writeProblem(w, 500, "build failed", err.Error())
		return
	}
	if err := s.carf.Add(rec); err != nil {
		writeProblem(w, 500, "persist failed", err.Error())
		return
	}
	writeJSON(w, 201, rec)
}

func (s *Service) handleCARFList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"messages": s.carf.List()})
}

func (s *Service) handleCARFGet(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.carf.Get(r.PathValue("id"))
	if !ok {
		writeProblem(w, 404, "not found", "message not found")
		return
	}
	writeJSON(w, 200, rec)
}

func (s *Service) handleCARFXML(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.carf.Get(r.PathValue("id"))
	if !ok {
		writeProblem(w, 404, "not found", "message not found")
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(rec.XML))
}

// handleCARFCorrect is the correction loop: builds an OECD2 correction
// message referencing the original (rp-carf-schema carf.correction.protocol).
func (s *Service) handleCARFCorrect(w http.ResponseWriter, r *http.Request) {
	orig, ok := s.carf.Get(r.PathValue("id"))
	if !ok {
		writeProblem(w, 404, "not found", "message not found")
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	if req.Reason == "" {
		writeProblem(w, 422, "validation failed", "correction reason required")
		return
	}
	rec, err := BuildCARFMessage(s, orig.TenantID, orig.Period, "OECD2", orig.MessageRefId, req.Reason)
	if err != nil {
		writeProblem(w, 500, "correction build failed", err.Error())
		return
	}
	orig.Status = "superseded"
	if err := s.carf.Add(orig); err != nil { // persist status transition
		writeProblem(w, 500, "persist failed", err.Error())
		return
	}
	if err := s.carf.Add(rec); err != nil {
		writeProblem(w, 500, "persist failed", err.Error())
		return
	}
	writeJSON(w, 201, rec)
}

// handleCARFTransmit enforces carf.transmit_enabled + carf.gate.changed:
// transmission refuses while either gate is closed (fail-safe).
func (s *Service) handleCARFTransmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MessageID string `json:"message_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	rec, ok := s.carf.Get(req.MessageID)
	if !ok {
		writeProblem(w, 404, "not found", "message not found")
		return
	}
	gates := s.gates.Gates()
	if !s.gates.Open("carf.transmit_enabled") || !s.gates.Open("carf.gate.changed") {
		rec.Status = "refused"
		s.carf.Add(rec) // persist status transition (best-effort; refusal still returned)
		writeProblem(w, 423, "transmission refused: gate closed",
			"carf.transmit_enabled="+boolStr(gates["carf.transmit_enabled"].Open)+
				" carf.gate.changed="+boolStr(gates["carf.gate.changed"].Open)+
				" (source: "+gates["carf.transmit_enabled"].Source+")")
		return
	}
	if len(rec.Validation) > 0 {
		writeProblem(w, 422, "message invalid", strings.Join(rec.Validation, "; "))
		return
	}
	rec.Status = "transmitted"
	if err := s.carf.Add(rec); err != nil { // persist status transition
		writeProblem(w, 500, "persist failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"status": "transmitted", "message_ref_id": rec.MessageRefId,
		"note": "dev simulator: envelope logged, no real OECD channel", "gates": gates})
}

func boolStr(b bool) string {
	if b {
		return "open"
	}
	return "closed"
}

func (s *Service) handleGates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"gates": s.gates.Gates()})
}

func (s *Service) handleGateFlip(w http.ResponseWriter, r *http.Request) {
	if !hasRole(claimsOf(r), "admin") {
		writeProblem(w, 403, "forbidden", "gate flip requires admin role (board-authorized)")
		return
	}
	var req struct {
		Open bool `json:"open"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	id := r.PathValue("id")
	if err := s.gates.SetLocal(id, req.Open); err != nil {
		writeProblem(w, 500, "flip failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"gate": id, "open": req.Open, "source": "local-file"})
}

func (s *Service) handleDuties(w http.ResponseWriter, r *http.Request) {
	var ctx map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&ctx); err != nil {
		writeProblem(w, 400, "bad request", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"duties": s.packs.DutiesFor(ctx), "pack": "rp-nta-vasp-duties"})
}

func (s *Service) handlePacks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"packs": s.packs.Loaded()})
}
