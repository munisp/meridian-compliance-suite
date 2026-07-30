package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type Config struct {
	Port       string
	AuthMode   string
	JWTSecret  string
	DataDir    string
	WORMURL    string // core audit-evidence; empty -> local WORM
	NotifyURL  string // core notification svc; empty -> log notifier
	WatchEvery time.Duration
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type Service struct {
	cfg     Config
	store   *Store
	rel     *RelationChecker
	worm    WORMClient
	notify  Notifier
	stopCh  chan struct{}
}

func main() {
	cfg := Config{
		Port:       env("PORT", "8113"),
		AuthMode:   env("AUTH_MODE", "dev"),
		JWTSecret:  env("MERIDIAN_DEV_JWT_SECRET", "meridian-dev-secret"),
		DataDir:    env("DATA_DIR", "./data"),
		WORMURL:    env("AUDIT_EVIDENCE_URL", ""),
		NotifyURL:  env("NOTIFICATION_SVC_URL", ""),
		WatchEvery: 30 * time.Second,
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("datadir: %v", err)
	}
	var worm WORMClient
	if cfg.WORMURL != "" {
		worm = &HTTPWORM{Base: cfg.WORMURL}
	} else {
		worm = &LocalWORM{Dir: cfg.DataDir}
	}
	var notify Notifier
	if cfg.NotifyURL != "" {
		notify = &HTTPNotifier{Base: cfg.NotifyURL}
	} else {
		notify = LogNotifier{}
	}
	svc := &Service{
		cfg: cfg, store: NewStore(cfg.DataDir),
		rel: NewRelationChecker(cfg.DataDir + "/relations.json"),
		worm: worm, notify: notify, stopCh: make(chan struct{}),
	}
	go svc.deadlineWatch()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "service": "case-mgmt", "version": "1.0.0"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ready"})
	})
	// matters
	mux.HandleFunc("POST /v1/matters", svc.auth(svc.handleCreateMatter))
	mux.HandleFunc("GET /v1/matters", svc.auth(svc.handleListMatters))
	mux.HandleFunc("GET /v1/matters/{id}", svc.auth(svc.handleGetMatter))
	mux.HandleFunc("PATCH /v1/matters/{id}", svc.auth(svc.handleUpdateMatter))
	// documents
	mux.HandleFunc("POST /v1/matters/{id}/documents", svc.auth(svc.handleUploadDoc))
	mux.HandleFunc("GET /v1/matters/{id}/documents", svc.auth(svc.handleListDocs))
	mux.HandleFunc("GET /v1/documents/{id}", svc.auth(svc.handleGetDoc))
	mux.HandleFunc("GET /v1/documents/{id}/content", svc.auth(svc.handleGetDocContent))
	// deadlines
	mux.HandleFunc("POST /v1/matters/{id}/deadlines", svc.auth(svc.handleCreateDeadline))
	mux.HandleFunc("GET /v1/deadlines", svc.auth(svc.handleListDeadlines))
	mux.HandleFunc("PATCH /v1/deadlines/{id}", svc.auth(svc.handleUpdateDeadline))
	// client portal (client-scoped views)
	mux.HandleFunc("GET /v1/portal/matters", svc.auth(svc.handlePortalMatters))
	mux.HandleFunc("GET /v1/portal/matters/{id}", svc.auth(svc.handlePortalMatter))
	// relations (dev permify checker)
	mux.HandleFunc("POST /v1/relations/check", svc.auth(svc.handleRelCheck))
	mux.HandleFunc("GET /v1/relations", svc.auth(svc.handleRelList))
	mux.HandleFunc("POST /v1/relations/grant", svc.auth(svc.handleRelGrant))
	mux.HandleFunc("POST /v1/relations/revoke", svc.auth(svc.handleRelRevoke))
	// evidence pack
	mux.HandleFunc("POST /v1/matters/{id}/evidence-pack", svc.auth(svc.handleEvidencePack))
	// workflows
	mux.HandleFunc("GET /v1/workflows", svc.auth(svc.handleWorkflowList))
	mux.HandleFunc("POST /v1/workflows/{name}/run", svc.auth(svc.handleWorkflowRun))

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: svc.recover(svc.logging(mux)), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("case-mgmt listening on :%s (auth=%s worm=%s notify=%s)",
		cfg.Port, cfg.AuthMode, orDef(cfg.WORMURL, "local"), orDef(cfg.NotifyURL, "log"))
	log.Fatal(srv.ListenAndServe())
}

func orDef(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (s *Service) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if strings.HasPrefix(h, "Bearer ") {
			claims, err := verifyHS256(strings.TrimPrefix(h, "Bearer "), s.cfg.JWTSecret)
			if err != nil {
				writeProblem(w, 401, "unauthorized", err.Error())
				return
			}
			r.Header.Set("X-Subject", "user:"+claims.Sub)
			next(w, r)
			return
		}
		if s.cfg.AuthMode == "dev" {
			if role := r.Header.Get("X-Dev-Role"); role != "" {
				subj := r.Header.Get("X-Dev-Subject")
				if subj == "" {
					subj = "dev-" + role
				}
				r.Header.Set("X-Subject", "user:"+subj)
				r.Header.Set("X-Role", role)
				next(w, r)
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
