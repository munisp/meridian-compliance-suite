package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Multi-APP pluggable router (SPEC §3 T1/T2): invoices are routed to an
// Access Point Provider (APP) per tenant. APPs are pluggable; the sandbox
// APP is the MBS simulator. Env APP_ROUTES="tenantA=mbs-sandbox,*=mbs-sandbox".

// APP is an access-point provider: a named MBSClient plus routing metadata.
type APP struct {
	ID     string     `json:"id"`
	Client MBSClient  `json:"-"`
	Kind   string     `json:"kind"` // simulator|http
}

// APPRouter resolves tenant → APP and fans out submissions.
type APPRouter struct {
	mu     sync.RWMutex
	apps   map[string]*APP
	routes map[string]string // tenantID (or "*") -> app id
}

func NewAPPRouter(defaultClient MBSClient) *APPRouter {
	r := &APPRouter{apps: map[string]*APP{}, routes: map[string]string{}}
	kind := "simulator"
	if _, ok := defaultClient.(*HTTPMBS); ok {
		kind = "http"
	}
	r.Register(&APP{ID: defaultClient.Name(), Client: defaultClient, Kind: kind})
	r.routes["*"] = defaultClient.Name()
	if routes := os.Getenv("APP_ROUTES"); routes != "" {
		for _, pair := range strings.Split(routes, ",") {
			kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
			if len(kv) == 2 {
				r.routes[kv[0]] = kv[1]
			}
		}
	}
	return r
}

// Register adds an APP to the registry (pluggable).
func (r *APPRouter) Register(app *APP) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.apps[app.ID] = app
}

// Route assigns a tenant to an APP id.
func (r *APPRouter) Route(tenantID, appID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[tenantID] = appID
}

// Resolve returns the APP for a tenant.
func (r *APPRouter) Resolve(tenantID string) (*APP, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	appID, ok := r.routes[tenantID]
	if !ok {
		appID = r.routes["*"]
	}
	app, ok := r.apps[appID]
	if !ok {
		return nil, fmt.Errorf("no APP %q for tenant %q", appID, tenantID)
	}
	return app, nil
}

// List describes registered APPs and routes.
func (r *APPRouter) List() map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	apps := []map[string]string{}
	for _, a := range r.apps {
		apps = append(apps, map[string]string{"id": a.ID, "kind": a.Kind})
	}
	routes := map[string]string{}
	for k, v := range r.routes {
		routes[k] = v
	}
	return map[string]any{"apps": apps, "routes": routes}
}

// Preclear routes a pre-clearance submission for the tenant.
func (r *APPRouter) Preclear(ctx context.Context, inv *CanonicalInvoice, ublXML []byte) (*ClearanceResult, string, error) {
	app, err := r.Resolve(inv.TenantID)
	if err != nil {
		return nil, "", err
	}
	res, err := app.Client.Preclear(ctx, inv, ublXML)
	return res, app.ID, err
}

// ReportB2C routes a B2C real-time report for the tenant.
func (r *APPRouter) ReportB2C(ctx context.Context, inv *CanonicalInvoice) (*B2CReportReceipt, string, error) {
	app, err := r.Resolve(inv.TenantID)
	if err != nil {
		return nil, "", err
	}
	res, err := app.Client.ReportB2C(ctx, inv)
	return res, app.ID, err
}
