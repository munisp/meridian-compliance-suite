"""OTel smoke tests (DESIGN-CONTRACT.md): init_otel boots fail-soft inside
the real service app, TenantBaggageMiddleware is installed, and a fresh
instrumented app emits a SERVER span carrying tenant.id."""

from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import (
    InMemorySpanExporter,
)


def test_service_app_boots_with_otel():
    # app.main import runs init_otel(app) with no OTLP endpoint in tests:
    # must not raise (fail-soft money-path rule).
    from app.main import app

    from meridian_py.otel import TenantBaggageMiddleware

    assert any(m.cls is TenantBaggageMiddleware for m in app.user_middleware)


def test_service_healthz_ok():
    from fastapi.testclient import TestClient

    from app.main import app

    r = TestClient(app).get("/healthz", headers={"X-Meridian-Tenant": "t-smoke"})
    assert r.status_code == 200


def test_span_smoke_tenant_attr():
    fastapi = __import__("fastapi")
    from fastapi.testclient import TestClient

    from meridian_py import otel

    exp = InMemorySpanExporter()
    tp = TracerProvider()
    tp.add_span_processor(SimpleSpanProcessor(exp))

    app = fastapi.FastAPI()

    @app.get("/v1/ping/{pid}")
    def ping(pid: str):
        return {"pid": pid}

    otel.init_otel(app, tracer_provider=tp)
    app.add_middleware(otel.TenantBaggageMiddleware)
    r = TestClient(app).get("/v1/ping/p1", headers={"X-Meridian-Tenant": "t-smoke"})
    assert r.status_code == 200

    spans = exp.get_finished_spans()
    server = [s for s in spans if s.kind == trace.SpanKind.SERVER]
    assert server, "no SERVER span recorded"
    tenants = {s.attributes.get("tenant.id") for s in server if s.attributes}
    assert "t-smoke" in tenants
    routes = {s.attributes.get("http.route") for s in server if s.attributes}
    assert any("/v1/ping/{pid}" in (rt or "") for rt in routes)
