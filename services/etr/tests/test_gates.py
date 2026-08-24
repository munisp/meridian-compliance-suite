"""Gate resolver tests (B2-#10): reg-watch armed/disarmed, local fallback, fail-closed."""
import json
import os
import sys

import httpx
import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from app import gates


@pytest.fixture(autouse=True)
def _clean_env(monkeypatch, tmp_path):
    monkeypatch.delenv("REG_WATCH_URL", raising=False)
    monkeypatch.delenv("REG_WATCH_TOKEN", raising=False)
    monkeypatch.setenv("ETR_GATE_FILE", str(tmp_path / "gates.json"))


def _mock_regwatch(state: str | None):
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path == "/v1/gates"
        g = [] if state is None else [{"id": "qdmtt_upgrade", "state": state}]
        return httpx.Response(200, json={"gates": g})
    return handler


def test_regwatch_armed(monkeypatch):
    monkeypatch.setenv("REG_WATCH_URL", "http://regwatch.test")
    monkeypatch.setattr(httpx, "get", lambda url, **kw: _mock_regwatch("armed")(httpx.Request("GET", url)))
    assert gates.qdmtt_upgrade_armed() == (True, "reg-watch")


def test_regwatch_disarmed(monkeypatch):
    monkeypatch.setenv("REG_WATCH_URL", "http://regwatch.test")
    monkeypatch.setattr(httpx, "get", lambda url, **kw: _mock_regwatch("disarmed")(httpx.Request("GET", url)))
    assert gates.qdmtt_upgrade_armed() == (False, "reg-watch")


def test_regwatch_unreachable_local_file(monkeypatch, tmp_path):
    monkeypatch.setenv("REG_WATCH_URL", "http://127.0.0.1:1")
    with open(tmp_path / "gates.json", "w") as fh:
        json.dump({"qdmtt_upgrade": True}, fh)
    assert gates.qdmtt_upgrade_armed() == (True, "local-file")


def test_fail_closed_default():
    assert gates.qdmtt_upgrade_armed() == (False, "default")
