// Middleware: OTel HTTP middleware for Go services (e.g. einvoicing).
// One SERVER span per request carrying:
//   - http.route = the TEMPLATED route (e.g. /v1/invoices/{id}), never the
//     raw path (low cardinality);
//   - tenant.id  = resolved tenant (header or JWT claim), propagated as
//     baggage for downstream calls;
//   - a W3C traceparent extracted from inbound headers (B3 propagation).
//
// Client: outbound RoundTripper that starts a CLIENT span per call and
// injects traceparent + baggage (tenant) into the outbound request.
//
// Design: no hard dependency on a collector — spans are no-ops when the
// global TracerProvider is unset (InitProviders installs a real one).
package otelx

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/munisp/meridian-compliance-suite/packages/otelx"

// Middleware returns an http.Handler that wraps next with a SERVER span.
// When next is a *http.ServeMux, the span is renamed to the matched route
// template (Go 1.22 mux gives the registered pattern via Pattern).
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(),
			propagationHeaderCarrier{h: r.Header})

		tenant := TenantFromRequest(r)
		if tenant != "" {
			if member, err := baggage.NewMember("tenant.id", tenant); err == nil {
				if bag, err := baggage.New(member); err == nil {
					ctx = baggage.ContextWithBaggage(ctx, bag)
				}
			}
		}

		tracer := otel.Tracer(tracerName)
		// The provisional span name/attributes use the REDACTED path only
		// (audit R4 #14): raw paths can embed PII (TINs in /v1/wht/credits/
		// <tin>, IRNs in /v1/invoices/<irn>) and must never reach a span.
		ctx, span := tracer.Start(ctx, r.Method+" "+redactPath(r.URL.Path),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				semconv.HTTPRequestMethodKey.String(r.Method),
				semconv.URLPath(redactPath(r.URL.Path)),
			))
		defer span.End()

		if tenant != "" {
			span.SetAttributes(attribute.String("tenant.id", tenant))
		}

		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		r2 := r.WithContext(ctx)
		next.ServeHTTP(rw, r2)

		// Low-cardinality route template (Go 1.22 mux knows the pattern).
		// Falls back to "unmatched" for 404s outside any registered route.
		route := ""
		if mux, ok := next.(*http.ServeMux); ok {
			_, route = mux.Handler(r2)
		}
		if route == "" {
			route = "unmatched"
		} else {
			// Go 1.22 ServeMux method patterns already carry the verb
			// ("GET /v1/..."): prefixing r.Method produced the
			// double-method span name "GET GET /v1/..." (audit R4 #14).
			route = stripMethodPrefix(route)
		}
		span.SetName(r.Method + " " + route)
		span.SetAttributes(
			semconv.HTTPRoute(route),
			semconv.HTTPResponseStatusCode(rw.status),
		)
		if rw.status >= 500 {
			span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", rw.status))
		} else {
			span.SetStatus(codes.Ok, "")
		}
	})
}

// Client wraps an http.RoundTripper with a CLIENT span and injects the
// W3C traceparent + tenant baggage into the outbound request headers.
func Client(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return roundTripper{base: base}
}

type roundTripper struct{ base http.RoundTripper }

func (rt roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	tracer := otel.Tracer(tracerName)
	// PII (audit R4 #14): the full URL carries the query string (tin=, irn=)
	// and paths can embed TINs/IRNs — record the redacted, query-free URL.
	safeURL := *req.URL
	safeURL.RawQuery, safeURL.Fragment = "", ""
	safeURL.Path = redactPath(safeURL.Path)
	safeURL.RawPath = ""
	ctx, span := tracer.Start(req.Context(), req.Method+" "+req.URL.Host+redactPath(req.URL.Path),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.HTTPRequestMethodKey.String(req.Method),
			semconv.URLFull(safeURL.String()),
			attribute.String("server.address", req.URL.Host),
		))
	defer span.End()

	r2 := req.Clone(ctx)
	otel.GetTextMapPropagator().Inject(ctx, propagationHeaderCarrier{h: r2.Header})

	resp, err := rt.base.RoundTrip(r2)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetAttributes(semconv.HTTPResponseStatusCode(resp.StatusCode))
	if resp.StatusCode >= 500 {
		span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", resp.StatusCode))
	}
	return resp, nil
}

// statusWriter captures the response status code.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher so streaming handlers (SSE) keep working.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// stripMethodPrefix removes the leading HTTP verb from a Go 1.22 ServeMux
// pattern ("GET /v1/x" -> "/v1/x"); host patterns are preserved.
func stripMethodPrefix(pattern string) string {
	if i := strings.IndexByte(pattern, ' '); i > 0 {
		switch pattern[:i] {
		case http.MethodGet, http.MethodHead, http.MethodPost,
			http.MethodPut, http.MethodPatch, http.MethodDelete,
			http.MethodConnect, http.MethodOptions, http.MethodTrace:
			return pattern[i+1:]
		}
	}
	return pattern
}

// redactPath replaces path segments that can carry taxpayer PII — TINs
// (digit-heavy, e.g. 1234567890123) and e-invoice IRNs
// (<number>-<serviceId8>-<yyyymmdd>) — with a placeholder, so TINs/IRNs
// never appear in span names or attributes (audit R4 #14).
var (
	tinSegment = regexp.MustCompile(`^[0-9][0-9-]{7,}$`)
	irnSegment = regexp.MustCompile(`^[^/]+-[^/-]{8}-[0-9]{8}$`)
)

func redactPath(p string) string {
	segs := strings.Split(p, "/")
	redacted := false
	for i, seg := range segs {
		if tinSegment.MatchString(seg) || irnSegment.MatchString(seg) {
			segs[i] = "{redacted}"
			redacted = true
		}
	}
	if !redacted {
		return p
	}
	return strings.Join(segs, "/")
}

// propagationHeaderCarrier adapts http.Header to propagation.TextMapCarrier.
type propagationHeaderCarrier struct{ h http.Header }

func (c propagationHeaderCarrier) Get(key string) string { return c.h.Get(key) }
func (c propagationHeaderCarrier) Set(key, value string) { c.h.Set(key, value) }
func (c propagationHeaderCarrier) Keys() []string {
	out := make([]string, 0, len(c.h))
	for k := range c.h {
		out = append(out, k)
	}
	return out
}
