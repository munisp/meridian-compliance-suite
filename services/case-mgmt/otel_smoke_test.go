package main

// otel_smoke_test.go — span smoke test (DESIGN-CONTRACT.md): the OTel
// middleware emits one SERVER span per request carrying tenant.id and the
// low-cardinality http.route template; disabled mode never fails.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/munisp/meridian-compliance-suite/packages/otelx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestOTelMiddlewareSpanSmoke(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/ping/abc", nil)
	req.Header.Set("X-Meridian-Tenant", "tenant-smoke-01")
	otelx.Middleware(mux).ServeHTTP(httptest.NewRecorder(), req)

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	var tenant, route string
	for _, a := range spans[0].Attributes {
		switch string(a.Key) {
		case "tenant.id":
			tenant = a.Value.AsString()
		case "http.route":
			route = a.Value.AsString()
		}
	}
	if tenant != "tenant-smoke-01" {
		t.Errorf("tenant.id = %q, want tenant-smoke-01", tenant)
	}
	if route != "GET /v1/ping/{id}" {
		t.Errorf("http.route = %q, want templated route", route)
	}
}

func TestOTelDisabledModeNoop(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("PROFILE", "prod") // prod without endpoint: loud warning, no failure
	p := otelx.InitProviders(context.Background())
	if p.Enabled() {
		t.Error("providers must be disabled without an OTLP endpoint")
	}
	p.Shutdown(context.Background())
}
