// Command pos-vat is the Meridian T6 POS VAT service: POS receipt ingest,
// VAT basket classification (standard_75 / zero_rated / exempt), capture-time
// state/LGA attribution, federal/state attribution switch with dual_shadow
// mode, store-and-forward spool, settlement recon posting to the VAT
// remittance ledger (core ledger svc, dev client fallback), and the wf-vat-*
// workflow set. Dev-standalone: zero external deps required (SPEC §1.3).
package main

import (
	"log"
	"net/http"

	"github.com/munisp/meridian-compliance-suite/packages/httpx"
	"os"
)

type Config struct {
	Port            string
	AuthMode        string
	JWTSecret       string
	GeoURL          string // core geo svc; empty -> embedded polygons
	LedgerURL       string // core ledger svc; empty -> dev in-mem ledger
	RegistryURL     string // rp-registry; empty -> embedded pack copies
	RedisURL        string // optional hot cache; empty -> in-mem
	DataDir         string // durable fallback store + spool dir
	AttributionMode string // overridden by rp-vat-attribution-mode when loaded
	EventBus        string
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	cfg := Config{
		Port:            env("PORT", "8106"),
		AuthMode:        env("AUTH_MODE", "dev"),
		JWTSecret:       env("MERIDIAN_DEV_JWT_SECRET", "meridian-dev-secret"),
		GeoURL:          env("GEO_SVC_URL", ""),
		LedgerURL:       env("LEDGER_SVC_URL", ""),
		RegistryURL:     env("RP_REGISTRY_URL", ""),
		RedisURL:        env("REDIS_URL", ""),
		DataDir:         env("DATA_DIR", "./data"),
		AttributionMode: env("ATTRIBUTION_MODE", "state"),
		EventBus:        env("EVENT_BUS", "inproc"),
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("datadir: %v", err)
	}

	svc := NewService(cfg)
	svc.packs.LoadPacks()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", svc.healthz)
	mux.HandleFunc("GET /readyz", svc.readyz)
	// ingest + query
	mux.HandleFunc("POST /v1/receipts", svc.auth(svc.handleIngestReceipt))
	mux.HandleFunc("GET /v1/receipts", svc.auth(svc.handleListReceipts))
	mux.HandleFunc("GET /v1/receipts/{id}", svc.auth(svc.handleGetReceipt))
	// spool (store-and-forward)
	mux.HandleFunc("GET /v1/spool", svc.auth(svc.handleListSpool))
	mux.HandleFunc("POST /v1/spool/drain", svc.auth(svc.handleSpoolDrain))
	// settlement recon + variance + b2c + cert
	mux.HandleFunc("POST /v1/settlement/recon", svc.auth(svc.handleSettlementRecon))
	mux.HandleFunc("GET /v1/settlement/recon", svc.auth(svc.handleListRecon))
	mux.HandleFunc("GET /v1/variance", svc.auth(svc.handleVariance))
	mux.HandleFunc("POST /v1/b2c/report", svc.auth(svc.handleB2CReport))
	mux.HandleFunc("POST /v1/cert-run", svc.auth(svc.handleCertRun))
	// attribution + packs + events
	mux.HandleFunc("GET /v1/attribution/mode", svc.auth(svc.handleAttributionMode))
	mux.HandleFunc("GET /v1/packs", svc.auth(svc.handlePacks))
	mux.HandleFunc("GET /v1/events", svc.auth(svc.handleEvents))
	// workflows
	mux.HandleFunc("POST /v1/workflows/{name}/run", svc.auth(svc.handleWorkflowRun))
	mux.HandleFunc("GET /v1/workflows", svc.auth(svc.handleWorkflowList))

	// F-5: graceful shutdown on SIGTERM/SIGINT + full server timeouts.
	srv := httpx.NewServer(":"+cfg.Port, svc.recoverMiddleware(svc.logging(mux)))
	log.Printf("pos-vat listening on :%s (auth=%s bus=%s ledger=%s geo=%s registry=%s redis=%s)",
		cfg.Port, cfg.AuthMode, cfg.EventBus, orElse(cfg.LedgerURL, "dev-inmem"), orElse(cfg.GeoURL, "embedded"),
		orElse(cfg.RegistryURL, "embedded"), orElse(cfg.RedisURL, "in-mem"))
	log.Fatal(httpx.Serve(srv))
}

func orElse(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
