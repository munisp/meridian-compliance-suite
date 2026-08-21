package main

// Stakeholder webhook notifications (NRS lifecycle step 7 — transmission).
// Per-business webhook endpoints, HMAC-SHA256 signature header
// (X-Meridian-Signature), retry with linear backoff. The default sink posts
// over HTTP; tests/dev can install an in-process sink. Registration is
// fail-closed in production (ENV=production): HTTPS URLs and a >= 16-byte
// secret are mandatory.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// WebhookEndpoint is one registered stakeholder callback.
type WebhookEndpoint struct {
	URL       string `json:"url"`
	Secret    string `json:"-"` // never serialised
	CreatedAt string `json:"created_at"`
}

// WebhookDelivery records one delivery attempt outcome (audit/debug).
type WebhookDelivery struct {
	URL       string `json:"url"`
	Event     string `json:"event"`
	Attempts  int    `json:"attempts"`
	Status    string `json:"status"` // delivered|failed
	LastError string `json:"last_error,omitempty"`
}

// WebhookSink performs the actual POST. The HTTP sink is the default; the
// in-process sink is for dev/tests.
type WebhookSink interface {
	Post(ctx context.Context, url string, body []byte, headers map[string]string) error
}

// HTTPWebhookSink posts real HTTP webhooks.
type HTTPWebhookSink struct{ Client *http.Client }

func (h *HTTPWebhookSink) Post(ctx context.Context, url string, body []byte, headers map[string]string) error {
	cli := h.Client
	if cli == nil {
		cli = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook %s returned %d", url, resp.StatusCode)
	}
	return nil
}

// InprocWebhookSink is the dev in-process sink: captures deliveries in memory.
type InprocWebhookSink struct {
	mu      sync.Mutex
	Bodies  [][]byte
	Headers []map[string]string
	URLs    []string
	Fail    error // when non-nil, every Post fails with this error
}

func (s *InprocWebhookSink) Post(ctx context.Context, url string, body []byte, headers map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Fail != nil {
		return s.Fail
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	s.Bodies = append(s.Bodies, cp)
	s.Headers = append(s.Headers, headers)
	s.URLs = append(s.URLs, url)
	return nil
}

// WebhookRegistry holds per-business webhook endpoints and delivers events.
type WebhookRegistry struct {
	mu         sync.RWMutex
	byBusiness map[string][]WebhookEndpoint
	owners     map[string]string // businessID -> owning tenant (A1-05 BOLA binding)
	deliveries []WebhookDelivery
	Sink       WebhookSink
}

func NewWebhookRegistry(sink WebhookSink) *WebhookRegistry {
	if sink == nil {
		sink = &HTTPWebhookSink{}
	}
	return &WebhookRegistry{byBusiness: map[string][]WebhookEndpoint{}, owners: map[string]string{}, Sink: sink}
}

// Owner returns the tenant that owns a business's webhook registrations
// ("" if unbound).
func (r *WebhookRegistry) Owner(businessID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.owners[businessID]
}

func prodEnv() bool {
	e := strings.ToLower(os.Getenv("ENV") + os.Getenv("APP_ENV"))
	return strings.Contains(e, "prod")
}

// devProfile reports whether non-prod conveniences (http callbacks, private
// egress targets) are permitted. PROFILE=prod or ENV/APP_ENV containing
// "prod" means production.
func devProfile() bool {
	return !prodEnv() && strings.ToLower(os.Getenv("PROFILE")) != "prod"
}

// isPrivateEgressIP reports whether ip is loopback, RFC1918, link-local, or
// unspecified — destinations a webhook callback must never reach in prod
// (SSRF guard, A1-05).
func isPrivateEgressIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// validateWebhookURL enforces the outbound-callback policy (A1-05 SSRF):
//   - scheme: https always; http only in dev
//   - WEBHOOK_URL_ALLOWLIST (comma-separated host suffixes), when set, is
//     an allowlist the callback host must match
//   - DNS-resolved addresses must not be loopback/RFC1918/link-local/
//     unspecified outside dev
func validateWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("webhook url invalid")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return fmt.Errorf("webhook url must be http(s)")
	}
	if scheme == "http" && !devProfile() {
		return fmt.Errorf("webhook url must be https in production")
	}
	host := strings.ToLower(u.Hostname())
	if allow := os.Getenv("WEBHOOK_URL_ALLOWLIST"); allow != "" {
		ok := false
		for _, suffix := range strings.Split(allow, ",") {
			suffix = strings.ToLower(strings.TrimSpace(suffix))
			if suffix != "" && (host == suffix || strings.HasSuffix(host, "."+suffix)) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("webhook host %s not in WEBHOOK_URL_ALLOWLIST", host)
		}
	}
	if !devProfile() {
		ips, err := net.LookupIP(host)
		if err != nil {
			return fmt.Errorf("webhook host does not resolve: %v", err)
		}
		for _, ip := range ips {
			if isPrivateEgressIP(ip) {
				return fmt.Errorf("webhook host %s resolves to a non-public address (%s); refused outside dev", host, ip)
			}
		}
	}
	return nil
}

// Register adds an endpoint for a business, bound to the owning tenant
// (A1-05 BOLA): ownerTenant is required; the first registration binds the
// business to that tenant and later registrations from other tenants are
// rejected. Fail-closed in production: HTTPS only and a strong secret
// required.
func (r *WebhookRegistry) Register(businessID, ownerTenant, urlStr, secret string) error {
	if strings.TrimSpace(businessID) == "" {
		return fmt.Errorf("business_id required")
	}
	if strings.TrimSpace(ownerTenant) == "" {
		return fmt.Errorf("owning tenant required")
	}
	if strings.TrimSpace(urlStr) == "" {
		return fmt.Errorf("webhook url required")
	}
	if err := validateWebhookURL(urlStr); err != nil {
		return err
	}
	if prodEnv() {
		if len(secret) < 16 {
			return fmt.Errorf("webhook secret must be >= 16 bytes in production")
		}
	}
	if secret == "" {
		// dev convenience: deterministic per-endpoint secret
		sum := sha256.Sum256([]byte("meridian-dev-webhook|" + businessID + "|" + urlStr))
		secret = hex.EncodeToString(sum[:8])
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if owner, bound := r.owners[businessID]; bound && owner != ownerTenant {
		return fmt.Errorf("business %s is owned by another tenant", businessID)
	}
	for _, ep := range r.byBusiness[businessID] {
		if ep.URL == urlStr {
			return fmt.Errorf("webhook %s already registered for %s", urlStr, businessID)
		}
	}
	r.owners[businessID] = ownerTenant
	r.byBusiness[businessID] = append(r.byBusiness[businessID], WebhookEndpoint{
		URL: urlStr, Secret: secret, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	return nil
}

// Endpoints lists a business's registered endpoints (secrets redacted).
func (r *WebhookRegistry) Endpoints(businessID string) []WebhookEndpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]WebhookEndpoint, len(r.byBusiness[businessID]))
	copy(out, r.byBusiness[businessID])
	return out
}

// Deliveries returns delivery history.
func (r *WebhookRegistry) Deliveries() []WebhookDelivery {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]WebhookDelivery, len(r.deliveries))
	copy(out, r.deliveries)
	return out
}

// SignWebhook computes the X-Meridian-Signature value (HMAC-SHA256 hex).
func SignWebhook(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyWebhookSignature checks a delivery signature (constant-time).
func VerifyWebhookSignature(secret string, body []byte, signature string) bool {
	want := SignWebhook(secret, body)
	return hmac.Equal([]byte(want), []byte(signature))
}

// Notify delivers event to every endpoint registered for the business, with
// HMAC signature header and retry-with-backoff (3 attempts). All delivery
// outcomes are recorded; an error is returned only if EVERY endpoint failed.
func (r *WebhookRegistry) Notify(ctx context.Context, businessID, event string, payload any) error {
	body, err := json.Marshal(map[string]any{
		"event": event, "business_id": businessID,
		"sent_at": time.Now().UTC().Format(time.RFC3339), "data": payload,
	})
	if err != nil {
		return err
	}
	r.mu.RLock()
	eps := make([]WebhookEndpoint, len(r.byBusiness[businessID]))
	copy(eps, r.byBusiness[businessID])
	r.mu.RUnlock()
	if len(eps) == 0 {
		return nil // no stakeholders registered — transmission step still succeeds
	}
	var failures int
	var lastErr error
	for _, ep := range eps {
		headers := map[string]string{
			"X-Meridian-Event":     event,
			"X-Meridian-Signature": SignWebhook(ep.Secret, body),
		}
		d := WebhookDelivery{URL: ep.URL, Event: event, Status: "delivered"}
		var postErr error
		for attempt := 1; attempt <= 3; attempt++ {
			d.Attempts = attempt
			if postErr = r.Sink.Post(ctx, ep.URL, body, headers); postErr == nil {
				break
			}
			select {
			case <-ctx.Done():
				postErr = ctx.Err()
			case <-time.After(time.Duration(attempt*50) * time.Millisecond):
			}
			if ctx.Err() != nil {
				break
			}
		}
		if postErr != nil {
			d.Status = "failed"
			d.LastError = postErr.Error()
			failures++
			lastErr = postErr
		}
		r.mu.Lock()
		r.deliveries = append(r.deliveries, d)
		r.mu.Unlock()
	}
	if failures == len(eps) {
		return fmt.Errorf("all %d webhook endpoints failed: %w", failures, lastErr)
	}
	return nil
}
