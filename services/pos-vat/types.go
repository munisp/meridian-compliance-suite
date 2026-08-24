package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/munisp/meridian-compliance-suite/packages/authx"
	"github.com/munisp/meridian-compliance-suite/packages/shared/rulepack"
)

// ---------- Money: integer kobo only (SPEC §1.3). Never floats. ----------

// ReceiptLine is one POS receipt line item.
type ReceiptLine struct {
	SKU         string `json:"sku"`
	Description string `json:"description"`
	Qty         int64  `json:"qty_milli"` // milli-units to stay integer
	UnitPrice   int64  `json:"unit_price_kobo"`
	Category    string `json:"category,omitempty"` // hint for basket classification
}

// Receipt is the canonical POS receipt (nrs.pos.receipts.v1 data payload).
type Receipt struct {
	ID              string        `json:"id"`
	TenantID        string        `json:"tenant_id"`
	MerchantTIN     string        `json:"merchant_tin"`
	MerchantTINHash string        `json:"merchant_tin_hash"`
	TerminalID      string        `json:"terminal_id"`
	ReceiptNo       string        `json:"receipt_no"`
	CapturedAt      string        `json:"captured_at"`
	Lat             float64       `json:"lat"`
	Lon             float64       `json:"lon"`
	Lines           []ReceiptLine `json:"lines"`
	Currency        string        `json:"currency"` // NGN
	IdempotencyKey  string        `json:"idempotency_key,omitempty"`
	// computed
	Baskets         map[string]int64  `json:"baskets"` // basket -> net kobo
	VATKobo         int64             `json:"vat_kobo"`
	TotalKobo       int64             `json:"total_kobo"`
	State           string            `json:"state"`
	LGA             string            `json:"lga"`
	Attribution     AttributionResult `json:"attribution"`
	Status          string            `json:"status"` // ingested|spooled|settled
	// SettledIn records the (tenant, period) settlement marker this receipt
	// was remitted under (B3 #2): receipts ingested after their period
	// settled stay unsettled until a supplemental settlement remits them.
	SettledIn       string            `json:"settled_in,omitempty"`
	RulePackVersion string            `json:"rule_pack_version"`
	// Citations: LCE SPEC §5 statute citations per computed VAT amount
	// (additive response-layer field; empty when no VAT-bearing basket).
	Citations []rulepack.Citation `json:"citations,omitempty"`
}

// AttributionResult holds federal/state/LGA attribution (+ dual_shadow pair).
// B3 #2: FederalKobo+StateKobo+LGAKobo always sums to the full vat_kobo
// (LGA receives the rounding remainder) — no share is ever dropped.
type AttributionResult struct {
	Mode          string `json:"mode"` // federal|state|dual_shadow
	FederalKobo   int64  `json:"federal_kobo"`
	StateKobo     int64  `json:"state_kobo"`
	LGAKobo       int64  `json:"lga_kobo"`
	State         string `json:"state,omitempty"`
	LGA           string `json:"lga,omitempty"`
	ShadowFederal int64  `json:"shadow_federal_kobo,omitempty"` // dual_shadow mirror
	ShadowState   int64  `json:"shadow_state_kobo,omitempty"`
	ShadowLGA     int64  `json:"shadow_lga_kobo,omitempty"`
}

// Envelope per SPEC §1.1.
type Envelope struct {
	ID              string          `json:"id"`
	Type            string          `json:"type"`
	Source          string          `json:"source"`
	Time            string          `json:"time"`
	TenantID        string          `json:"tenant_id"`
	TraceID         string          `json:"trace_id"`
	RulePackVersion string          `json:"rule_pack_version"`
	Data            json.RawMessage `json:"data"`
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	ulidMu   sync.Mutex
	ulidLast int64
)

// ULID returns a 26-char Crockford base32 ULID.
func ULID() string {
	ulidMu.Lock()
	defer ulidMu.Unlock()
	now := time.Now().UnixMilli()
	if now < ulidLast {
		now = ulidLast
	}
	ulidLast = now
	var b [16]byte
	binary.BigEndian.PutUint64(b[0:8], uint64(now))
	rand.Read(b[8:])
	const enc = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	_ = enc
	var out [26]byte
	// encode 128 bits as 26 base32 chars
	bits := make([]byte, 0, 130)
	for _, by := range b {
		for i := 7; i >= 0; i-- {
			bits = append(bits, (by>>uint(i))&1)
		}
	}
	for i := 0; i < 26; i++ {
		var v byte
		for j := 0; j < 5; j++ {
			idx := i*5 + j
			if idx < len(bits) {
				v = v<<1 | bits[idx]
			} else {
				v <<= 1
			}
		}
		out[i] = crockford[v]
	}
	return string(out[:])
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// TINHash pseudonymises a TIN (SPEC §1.3).
func TINHash(tin, key string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(tin))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

// ---------- RFC7807 problem+json ----------

type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeProblem(w http.ResponseWriter, code int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(Problem{
		Type:  fmt.Sprintf("https://meridian.ng/problems/%d", code),
		Title: title, Status: code, Detail: detail,
	})
}

// ---------- Dev JWT (HS256) auth (SPEC §1.3) ----------

type Claims struct {
	Sub      string   `json:"sub"`
	Roles    []string `json:"roles"`
	TenantID string   `json:"tenant_id"`
	Exp      int64    `json:"exp"`
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func verifyHS256(token, secret string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed token")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(mac.Sum(nil), sig) {
		return nil, fmt.Errorf("bad signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, err
	}
	if c.Exp != 0 && time.Now().Unix() > c.Exp {
		return nil, fmt.Errorf("token expired")
	}
	return &c, nil
}

type ctxKey string

const claimsKey ctxKey = "claims"

// ---------- Service ----------

type Service struct {
	cfg    Config
	store  *Store
	geo    GeoClient
	ledger LedgerClient
	packs  *PackSet
	bus    *InprocBus
	cache  Cache // redis or in-mem idempotency/hot cache
	http   *http.Client
	tinKey string // resolved TIN pseudonymisation HMAC key (A1-02)
}

// resolveTINKey resolves the TIN pseudonymisation HMAC key. Fail-closed
// (A1-02): in prod (PROFILE=prod or AUTH_MODE=keycloak) an explicit,
// non-default TIN_HMAC_KEY is required — the public dev default would make
// TIN hashes reversible by dictionary attack.
func resolveTINKey(cfg Config) (string, error) {
	key := os.Getenv("TIN_HMAC_KEY")
	prod := os.Getenv("PROFILE") == "prod" || cfg.AuthMode == "keycloak"
	if key == "" || key == "meridian-dev-tin-key" {
		if prod {
			return "", fmt.Errorf("PROFILE=prod/AUTH_MODE=keycloak requires an explicit non-default TIN_HMAC_KEY; refusing to start with the public dev key")
		}
		key = "meridian-dev-tin-key"
	}
	return key, nil
}

func NewService(cfg Config) *Service {
	tinKey, err := resolveTINKey(cfg)
	if err != nil {
		log.Fatalf("pos-vat: %v", err)
	}
	var cache Cache
	if cfg.RedisURL != "" {
		rc := NewRedisCache(cfg.RedisURL)
		if rc.Ping() == nil {
			cache = rc
			logm("info", "redis hot cache connected")
		} else {
			cache = NewMemCache()
		}
	} else {
		cache = NewMemCache()
	}
	var geo GeoClient
	if cfg.GeoURL != "" {
		geo = &HTTPGeoClient{Base: cfg.GeoURL}
	} else {
		geo = EmbeddedGeo{}
	}
	var ledger LedgerClient
	if cfg.LedgerURL != "" {
		ledger = &HTTPLedger{Base: cfg.LedgerURL}
	} else {
		ledger = NewDevLedger()
	}
	return &Service{
		cfg: cfg, store: NewStore(cfg.DataDir), geo: geo, ledger: ledger,
		packs: NewPackSet(cfg), bus: NewInprocBus(), cache: cache,
		http: &http.Client{Timeout: 4 * time.Second}, tinKey: tinKey,
	}
}

func logm(level, msg string) { fmt.Printf("[%s] %s %s\n", nowRFC3339(), level, msg) }

func (s *Service) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok", "service": "pos-vat", "version": "1.0.0"})
}

func (s *Service) readyz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"status": "ready", "packs_loaded": len(s.packs.Loaded())})
}

// auth implements SPEC §1.3: AUTH_MODE=keycloak verifies RS256 Bearer tokens
// against the realm JWKS (KEYCLOAK_ISSUER / KEYCLOAK_AUDIENCE /
// KEYCLOAK_JWKS_URL) via the shared authx verifier and FAILS CLOSED at route
// registration (startup) when the OIDC config is missing — there is no dev
// fallback. AUTH_MODE=dev (default) accepts HS256 Bearer tokens plus an
// allowlisted X-Dev-Role header.
var (
	kcOnce     sync.Once
	kcVerifier *authx.KeycloakVerifier
)

func (s *Service) auth(next http.HandlerFunc) http.HandlerFunc {
	if s.cfg.AuthMode == "keycloak" {
		// Runs at route registration (startup): refuse to serve when a
		// keycloak deployment is missing its OIDC configuration.
		kcOnce.Do(func() {
			kcVerifier = authx.KeycloakVerifierFromEnv()
			if kcVerifier == nil {
				log.Fatal("AUTH_MODE=keycloak but KEYCLOAK_ISSUER is unset; refusing to start (no dev fallback)")
			}
			log.Printf("profile=prod component=auth (keycloak issuer=%s)", kcVerifier.Issuer)
		})
	}
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if strings.HasPrefix(h, "Bearer ") {
			tok := strings.TrimPrefix(h, "Bearer ")
			if s.cfg.AuthMode == "keycloak" {
				c, err := kcVerifier.Verify(tok)
				if err != nil {
					writeProblem(w, 401, "unauthorized", err.Error())
					return
				}
				r = r.WithContext(contextWith(r, &Claims{Sub: c.Sub, Roles: c.Roles, TenantID: c.TenantID, Exp: c.Exp}))
				next(w, r)
				return
			}
			c, err := verifyHS256(tok, s.cfg.JWTSecret)
			if err != nil {
				writeProblem(w, 401, "unauthorized", err.Error())
				return
			}
			r = r.WithContext(contextWith(r, c))
			next(w, r)
			return
		}
		if s.cfg.AuthMode == "dev" {
			switch role := r.Header.Get("X-Dev-Role"); role {
			case "admin", "operator", "auditor":
				r = r.WithContext(contextWith(r, &Claims{Sub: "dev-" + role, Roles: []string{role}, TenantID: r.Header.Get("X-Tenant-ID")}))
				next(w, r)
				return
			}
		}
		writeProblem(w, 401, "unauthorized", "Bearer JWT or X-Dev-Role (dev mode) required")
	}
}

func claimsOf(r *http.Request) *Claims {
	if c, ok := r.Context().Value(claimsKey).(*Claims); ok {
		return c
	}
	return &Claims{Sub: "anon"}
}

func (s *Service) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/healthz" {
			logm("info", fmt.Sprintf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Microsecond)))
		}
	})
}

func (s *Service) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				writeProblem(w, 500, "internal error", fmt.Sprintf("%v", rec))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
