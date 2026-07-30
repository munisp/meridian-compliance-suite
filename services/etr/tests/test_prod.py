"""Prod-profile tests (HARDENING.md H1/H2): Keycloak RS256/JWKS auth with a
mocked JWKS client, and the DATABASE_URL Postgres store path (psycopg faked
by a sqlite-backed stub translating the paramstyle, so the PG code path —
SQL, upserts, ordering — is genuinely exercised)."""
import os
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
os.environ["ETR_DB"] = ":memory:"

import jwt as pyjwt
import pytest
from cryptography.hazmat.primitives.asymmetric import rsa
from fastapi.testclient import TestClient

from app import main
from app.main import app

ISSUER = "https://keycloak:8443/realms/meridian"
AUD = "meridian-services"

_priv = rsa.generate_private_key(public_exponent=65537, key_size=2048)


def make_token(**over):
    claims = {
        "sub": "svc-account", "iss": ISSUER, "aud": AUD,
        "exp": int(time.time()) + 3600, "iat": int(time.time()),
        "realm_access": {"roles": ["operator"]},
        "resource_access": {AUD: {"roles": ["etr-service"]}},
    }
    claims.update(over)
    return pyjwt.encode(claims, _priv, algorithm="RS256", headers={"kid": "k1"})


class _FakeSigningKey:
    key = _priv.public_key()


class _FakeJWKClient:
    def __init__(self, url):
        self.url = url

    def get_signing_key_from_jwt(self, token):
        return _FakeSigningKey()


@pytest.fixture()
def keycloak_mode(monkeypatch):
    monkeypatch.setenv("AUTH_MODE", "keycloak")
    monkeypatch.setenv("KEYCLOAK_ISSUER", ISSUER)
    monkeypatch.setenv("KEYCLOAK_AUDIENCE", AUD)
    monkeypatch.delenv("KEYCLOAK_JWKS_URL", raising=False)
    monkeypatch.setattr(main, "_jwks_client", None)
    monkeypatch.setattr(pyjwt, "PyJWKClient", _FakeJWKClient)
    yield
    monkeypatch.setattr(main, "_jwks_client", None)


def test_keycloak_valid_token(keycloak_mode):
    r = TestClient(app).get("/v1/packs", headers={"Authorization": f"Bearer {make_token()}"})
    assert r.status_code == 200


def test_keycloak_role_mapping(keycloak_mode):
    claims = main._verify_keycloak(make_token())
    assert claims["sub"] == "svc-account"
    assert set(claims["roles"]) == {"operator", "etr-service"}


def test_keycloak_rejects_bad_audience(keycloak_mode):
    r = TestClient(app).get("/v1/packs", headers={"Authorization": f"Bearer {make_token(aud='other')}"})
    assert r.status_code == 401


def test_keycloak_rejects_bad_issuer(keycloak_mode):
    r = TestClient(app).get("/v1/packs",
                            headers={"Authorization": f"Bearer {make_token(iss='https://evil')}"})
    assert r.status_code == 401


def test_keycloak_rejects_expired(keycloak_mode):
    tok = make_token(exp=int(time.time()) - 60, iat=int(time.time()) - 120)
    r = TestClient(app).get("/v1/packs", headers={"Authorization": f"Bearer {tok}"})
    assert r.status_code == 401


def test_keycloak_rejects_dev_role_header(keycloak_mode):
    r = TestClient(app).get("/v1/packs", headers={"X-Dev-Role": "admin"})
    assert r.status_code == 401


# ---------- Postgres store path ----------

class _FakePGCursor:
    """sqlite3-backed cursor that accepts the psycopg paramstyle (%s) and
    sqlite-compatible upsert syntax, returning dict rows like dict_row."""

    def __init__(self, conn):
        self._conn = conn
        self._rows = []
        self.description = None

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    @staticmethod
    def _tr(q: str) -> str:
        return (q.replace("%s", "?")
                 .replace("EXCLUDED.", "excluded.")
                 .replace("BIGSERIAL", "INTEGER"))

    def execute(self, q, params=()):
        q = self._tr(q)
        cur = self._conn.execute(q, params)
        self.description = cur.description
        self._rows = cur.fetchall() if cur.description else []

    def fetchall(self):
        return self._rows


class _FakePGConn:
    def __init__(self):
        import sqlite3
        self._conn = sqlite3.connect(":memory:", check_same_thread=False)
        self._conn.row_factory = sqlite3.Row

    def cursor(self):
        return _FakePGCursor(self._conn)


def test_postgres_backend_selected_and_roundtrip(monkeypatch, tmp_path):
    monkeypatch.setenv("DATABASE_URL", "postgres://u:p@db:5432/etr")
    import app.store as store_mod

    fake = _FakePGConn()
    import psycopg
    monkeypatch.setattr(psycopg, "connect", lambda *a, **k: fake)

    from app.models import Computation, ConstituentEntity, Group
    from app.store import Store, _PG_DDL

    # apply PG DDL against the fake (BIGSERIAL already translated in cursor)
    with fake.cursor() as cur:
        for stmt in _PG_DDL.split(";"):
            if stmt.strip():
                cur.execute(stmt)

    s = Store()
    assert s.backend == "postgres"

    s.put_group(Group(id="g1", name="PG MNE", consolidated_revenue_kobo=9_000_000_000_000_000))
    assert s.get_group("g1").name == "PG MNE"

    s.put_entities([ConstituentEntity(id="e1", group_id="g1", name="E1", jurisdiction="NG",
                                      is_upe=True, net_income_kobo=100, covered_taxes_kobo=30)])
    assert [e.id for e in s.entities("g1")] == ["e1"]
    assert len(s.all_entities()) == 1

    comp = Computation(id="c1", group_id="g1", fiscal_year=2025, basis="topup",
                       created_at="2026-01-01T00:00:00Z", total_topup_kobo=42,
                       in_scope=True, scope_reason="test", qdmtt_upgrade=False,
                       jurisdictions=[], iir_allocations=[], cfc_pool_kobo=0,
                       trace=[], pack_versions={})
    s.put_computation(comp)
    assert s.get_computation("c1").total_topup_kobo == 42
    listed = s.list_computations("g1")
    assert listed and listed[0]["id"] == "c1" and listed[0]["total_topup_kobo"] == 42


def test_store_dev_default(monkeypatch):
    monkeypatch.delenv("DATABASE_URL", raising=False)
    from app.store import Store
    s = Store()
    assert s.backend == "sqlite"


def test_store_falls_back_when_pg_unreachable(monkeypatch):
    monkeypatch.setenv("DATABASE_URL", "postgres://u:p@db:5432/etr")
    import psycopg
    monkeypatch.setattr(psycopg, "connect", lambda *a, **k: (_ for _ in ()).throw(OSError("refused")))
    from app.store import Store
    s = Store()
    assert s.backend == "sqlite"
