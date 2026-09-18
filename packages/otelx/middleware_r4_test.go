package otelx

// middleware_r4_test.go — regression (audit R4 #14):
//  1. Go 1.22 ServeMux method patterns ("GET /v1/...") produced the
//     double-method span name "GET GET /v1/..." — now stripped to
//     "GET /v1/...".
//  2. PII: TINs and e-invoice IRNs embedded in request paths, and query
//     strings on outbound calls, leaked into span names/attributes — now
//     redacted/omitted.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

func TestServerSpanNameSingleMethod(t *testing.T) {
	exp := setupRecorder(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/invoices/{irn}", okHandler)
	h := Middleware(mux)
	req := httptest.NewRequest("GET", "/v1/invoices/94ND90NR-BRANCH01-20260127", nil)
	req.Header.Set("X-Meridian-Tenant", "tenant-1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans=%d", len(spans))
	}
	if got := spans[0].Name; got != "GET /v1/invoices/{irn}" {
		t.Fatalf("span name %q (want single-method route template)", got)
	}
	// PII: the raw IRN must not appear in any attribute.
	for _, attr := range spans[0].Attributes {
		if attr.Key == "url.path" {
			if got := fmt.Sprint(attr.Value.AsString()); got == "/v1/invoices/94ND90NR-BRANCH01-20260127" {
				t.Fatalf("url.path attribute leaked raw IRN path: %q", got)
			}
		}
	}
}

func TestServerSpanPIIPathRedacted(t *testing.T) {
	exp := setupRecorder(t)
	mux := http.NewServeMux()
	// no matching route -> "unmatched"; provisional name/attrs used the raw
	// path (TIN leak) before the fix.
	h := Middleware(mux)
	req := httptest.NewRequest("GET", "/v1/wht/credits/1234567890123", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans=%d", len(spans))
	}
	for _, attr := range spans[0].Attributes {
		if v := attr.Value.AsString(); containsPII(v) {
			t.Fatalf("PII leaked into span attribute %v=%q", attr.Key, v)
		}
	}
}

func containsPII(v string) bool {
	for _, pii := range []string{"1234567890123", "94ND90NR-BRANCH01-20260127"} {
		if len(v) >= len(pii) && (v == pii || stringContains(v, pii)) {
			return true
		}
	}
	return false
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestClientSpanQueryAndTINRedacted(t *testing.T) {
	exp := setupRecorder(t)
	rt := Client(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody,
			Header: http.Header{}}, nil
	}))
	req, _ := http.NewRequest("GET",
		"https://vendor.example/v1/wht/credits/1234567890123?tin=1234567890123&irn=94ND90NR-BRANCH01-20260127", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans=%d", len(spans))
	}
	if stringContains(spans[0].Name, "1234567890123") {
		t.Fatalf("client span name leaked TIN: %q", spans[0].Name)
	}
	for _, attr := range spans[0].Attributes {
		v := attr.Value.AsString()
		if stringContains(v, "1234567890123") || stringContains(v, "tin=") || stringContains(v, "irn=") {
			t.Fatalf("PII/query leaked into client span attribute %v=%q", attr.Key, v)
		}
		if attr.Key == attribute.Key("url.full") && stringContains(v, "?") {
			t.Fatalf("url.full kept the query string: %q", v)
		}
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
