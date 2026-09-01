package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"


	"github.com/munisp/meridian-compliance-suite/packages/otelx"
)

// NRS live-rail port (Feature I1): structured port of the NRS/Gention Global
// Resources v1.1 endpoint map + auth catalog documented in nactp
// (services/nrs-client/firs/nrs_client.py) into the einvoicing MBS adapter so
// sandbox→live cutover is a config flip (MBS_PROFILE=live), not a code change.
//
// No secrets are hardcoded: all credentials come from the environment and the
// live profile fails closed (boot refusal) when required config is missing.

// NRSEndpoints is the Gention v1.1 endpoint catalog (paths relative to the
// rail base URL). Values mirror the nactp reference map 1:1.
type NRSEndpoints struct {
	// Auth token flow: POST AuthLogin {email, password} → data.access_token
	// (JWT), then Authorization: Bearer <token> on all subsequent calls.
	AuthLogin string
	// Invoice lifecycle.
	InvoiceUpload string // POST
	InvoiceUpdate string // PUT/PATCH, "{irn}" path template
	InvoiceStatus string // GET, "{irn}" path template
	InvoiceList   string // GET
	// Reference resource catalogs.
	TaxCategories string
	InvoiceTypes  string
	PaymentMeans  string
	ServiceCodes  string
	ProductCodes  string
	LGAs          string
	Currencies    string
	States        string
	Countries     string
}

// NRSEndpointCatalog returns the NRS/Gention v1.1 endpoint map.
func NRSEndpointCatalog() NRSEndpoints {
	return NRSEndpoints{
		AuthLogin:     "/api/v1/auth/login",
		InvoiceUpload: "/api/v1/invoice/upload",
		InvoiceUpdate: "/api/v1/invoice/update/{irn}",
		InvoiceStatus: "/api/v1/invoice/{irn}",
		InvoiceList:   "/api/v1/invoice/list",
		TaxCategories: "/api/v1/resources/tax-categories",
		InvoiceTypes:  "/api/v1/resources/invoice-types",
		PaymentMeans:  "/api/v1/resources/payment-means",
		ServiceCodes:  "/api/v1/resources/service-codes",
		ProductCodes:  "/api/v1/resources/product-codes",
		LGAs:          "/api/v1/resources/lgas",
		Currencies:    "/api/v1/resources/currencies",
		States:        "/api/v1/resources/states",
		Countries:     "/api/v1/resources/countries",
	}
}

// InvoiceUpdatePath resolves the update path for an IRN.
func (e NRSEndpoints) InvoiceUpdatePath(irn string) string {
	return strings.ReplaceAll(e.InvoiceUpdate, "{irn}", irn)
}

// InvoiceStatusPath resolves the status/get path for an IRN.
func (e NRSEndpoints) InvoiceStatusPath(irn string) string {
	return strings.ReplaceAll(e.InvoiceStatus, "{irn}", irn)
}

// Gention access-point auth header names (nactp auth catalog).
const (
	liveRailHeaderAPIKey    = "x-api-key"
	liveRailHeaderAPISecret = "x-api-secret"
	liveRailHeaderAuth      = "Authorization"
)

// LiveRailConfig carries the live NRS/Gention rail configuration. Every field
// is sourced from the environment; nothing is baked into the binary.
type LiveRailConfig struct {
	BaseURL   string // MBS_LIVE_BASE_URL, fallback NRS_BASE_URL
	APIKey    string // MBS_LIVE_API_KEY, fallback NRS_API_KEY
	APISecret string // MBS_LIVE_API_SECRET, fallback NRS_API_SECRET
	Email     string // MBS_LIVE_EMAIL, fallback NRS_EMAIL (auth login flow)
	Password  string // MBS_LIVE_PASSWORD, fallback NRS_PASSWORD
	ServiceID string // NRS_SERVICE_ID (optional; invoice ServiceID wins)
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// LiveRailConfigFromEnv loads and validates the live-rail config. It is
// fail-closed: any missing required field is an error.
func LiveRailConfigFromEnv() (LiveRailConfig, error) {
	cfg := LiveRailConfig{
		BaseURL:   strings.TrimRight(firstEnv("MBS_LIVE_BASE_URL", "NRS_BASE_URL"), "/"),
		APIKey:    firstEnv("MBS_LIVE_API_KEY", "NRS_API_KEY"),
		APISecret: firstEnv("MBS_LIVE_API_SECRET", "NRS_API_SECRET"),
		Email:     firstEnv("MBS_LIVE_EMAIL", "NRS_EMAIL"),
		Password:  firstEnv("MBS_LIVE_PASSWORD", "NRS_PASSWORD"),
		ServiceID: firstEnv("NRS_SERVICE_ID"),
	}
	var missing []string
	if cfg.BaseURL == "" {
		missing = append(missing, "MBS_LIVE_BASE_URL")
	}
	if cfg.APIKey == "" {
		missing = append(missing, "MBS_LIVE_API_KEY")
	}
	if cfg.APISecret == "" {
		missing = append(missing, "MBS_LIVE_API_SECRET")
	}
	if cfg.Email == "" {
		missing = append(missing, "MBS_LIVE_EMAIL")
	}
	if cfg.Password == "" {
		missing = append(missing, "MBS_LIVE_PASSWORD")
	}
	if len(missing) > 0 {
		return LiveRailConfig{}, fmt.Errorf("MBS_PROFILE=live requires %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}

// LiveRailClient is the MBSClient adapter for the real NRS/Gention rail.
// It authenticates lazily (JWT via the auth login endpoint) and stamps each
// request with the Gention access-point headers (x-api-key / x-api-secret /
// Authorization: Bearer).
type LiveRailClient struct {
	cfg       LiveRailConfig
	endpoints NRSEndpoints
	client    *http.Client

	mu    sync.Mutex
	token string
}

// NewLiveRailClient builds the live-rail adapter from a validated config.
func NewLiveRailClient(cfg LiveRailConfig) *LiveRailClient {
	return &LiveRailClient{cfg: cfg, endpoints: NRSEndpointCatalog()}
}

func (c *LiveRailClient) Name() string { return "mbs-live-rail" }

func (c *LiveRailClient) http() *http.Client {
	if c.client != nil {
		return c.client
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: otelx.Client(nil)}
}

// login runs the Gention auth token flow: POST {email, password} →
// data.access_token (JWT).
func (c *LiveRailClient) login(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{
		"email":    c.cfg.Email,
		"password": c.cfg.Password,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+c.endpoints.AuthLogin, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("nrs login decode: %w", err)
	}
	if resp.StatusCode != http.StatusOK || out.Data.AccessToken == "" {
		return fmt.Errorf("nrs login failed: status %d", resp.StatusCode)
	}
	c.mu.Lock()
	c.token = out.Data.AccessToken
	c.mu.Unlock()
	return nil
}

// accessToken returns the cached JWT, logging in on first use.
func (c *LiveRailClient) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok != "" {
		return tok, nil
	}
	if err := c.login(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" {
		return "", errors.New("nrs login produced no access token")
	}
	return c.token, nil
}

// newRequest builds an authenticated rail request with the Gention headers.
func (c *LiveRailClient) newRequest(ctx context.Context, method, path string, payload any) (*http.Request, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	var rdr *bytes.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(liveRailHeaderAPIKey, c.cfg.APIKey)
	req.Header.Set(liveRailHeaderAPISecret, c.cfg.APISecret)
	req.Header.Set(liveRailHeaderAuth, "Bearer "+tok)
	return req, nil
}

func (c *LiveRailClient) do(req *http.Request, out any) error {
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("nrs rail %s %s: status %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Preclear maps MBS pre-clearance onto the NRS invoice upload endpoint: the
// signed invoice (canonical model + UBL XML) is submitted for clearance and
// the rail's IRN/crypto-stamp response is decoded into ClearanceResult.
func (c *LiveRailClient) Preclear(ctx context.Context, inv *CanonicalInvoice, ublXML []byte) (*ClearanceResult, error) {
	req, err := c.newRequest(ctx, http.MethodPost, c.endpoints.InvoiceUpload,
		map[string]any{"invoice": inv, "ubl_xml": string(ublXML)})
	if err != nil {
		return nil, err
	}
	var out struct {
		Data ClearanceResult `json:"data"`
	}
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	res := out.Data
	if res.IRN == "" {
		res.IRN = inv.IRN
	}
	if res.Status == "" {
		res.Status = "cleared"
	}
	return &res, nil
}

// ReportB2C maps a real-time B2C fiscalisation report onto the same NRS
// invoice upload endpoint (NRS treats B2C reports as real-time submissions).
func (c *LiveRailClient) ReportB2C(ctx context.Context, inv *CanonicalInvoice) (*B2CReportReceipt, error) {
	req, err := c.newRequest(ctx, http.MethodPost, c.endpoints.InvoiceUpload,
		map[string]any{"invoice": inv, "kind": "B2C", "realtime": true})
	if err != nil {
		return nil, err
	}
	var out struct {
		Data B2CReportReceipt `json:"data"`
	}
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	rec := out.Data
	if rec.IRN == "" {
		rec.IRN = inv.IRN
	}
	if rec.Status == "" {
		rec.Status = "accepted"
	}
	return &rec, nil
}

// mbsProfileEnv is the rail selector: sandbox|live. Unset preserves the
// legacy MBS_BASE_URL gate.
const mbsProfileEnv = "MBS_PROFILE"

// selectMBSProfile resolves MBS_PROFILE, trimming whitespace.
func selectMBSProfile() string { return strings.ToLower(strings.TrimSpace(os.Getenv(mbsProfileEnv))) }

// newLiveRailOrFatal loads the live profile config, refusing to boot when it
// is incomplete (fail-closed in every profile, always fail-closed in prod).
func newLiveRailOrFatal() MBSClient {
	cfg, err := LiveRailConfigFromEnv()
	if err != nil {
		log.Fatalf("MBS FATAL: %v", err)
	}
	log.Printf("component=mbs-adapter rail=live base_url=%s", cfg.BaseURL)
	return NewLiveRailClient(cfg)
}
