"""rp-* pack loading for the ETR service: rp-registry API + embedded fallback.

Loads rp-etr-nta, rp-etr-scope, rp-etr-cfc, rp-globe-oecd, rp-gir-schema per
SPEC §1.4. Offline-resilient: embedded copies are pinned core contracts v1.
"""
from __future__ import annotations

import os
from pathlib import Path
from typing import Any

import httpx
import yaml

PACK_IDS = ["rp-etr-nta", "rp-etr-scope", "rp-etr-cfc", "rp-globe-oecd", "rp-gir-schema"]
PACKS_DIR = Path(__file__).resolve().parent.parent / "packs"


class PackSet:
    def __init__(self, registry_url: str = "") -> None:
        self.registry_url = registry_url
        self.packs: dict[str, dict[str, Any]] = {}
        self.sources: dict[str, str] = {}

    def load_all(self) -> None:
        for pid in PACK_IDS:
            pack, source = self._load_one(pid)
            if pack:
                self.packs[pid] = pack
                self.sources[pid] = source

    def _load_one(self, pid: str) -> tuple[dict[str, Any] | None, str]:
        if self.registry_url:
            try:
                resp = httpx.get(f"{self.registry_url}/v1/packs/{pid}/latest", timeout=2.0)
                if resp.status_code == 200:
                    body = resp.text
                    try:
                        data = resp.json()
                        body = data.get("yaml") or data.get("raw") or data.get("content") or body
                    except Exception:
                        pass
                    pack = yaml.safe_load(body)
                    if isinstance(pack, dict) and pack.get("id"):
                        return pack, "registry"
            except Exception:
                pass
        path = PACKS_DIR / f"{pid}.yaml"
        if path.exists():
            return yaml.safe_load(path.read_text()), "embedded"
        return None, "missing"

    def get(self, pid: str) -> dict[str, Any]:
        return self.packs.get(pid, {})

    def rules(self, pid: str) -> list[dict[str, Any]]:
        return self.get(pid).get("rules", []) or []

    def rule(self, pid: str, rule_id: str) -> dict[str, Any]:
        for r in self.rules(pid):
            if r.get("id") == rule_id:
                return r.get("then", {}) or {}
        return {}

    def versions(self) -> dict[str, str]:
        return {pid: f"{self.packs[pid].get('version', '?')} ({self.sources.get(pid, '?')})"
                for pid in sorted(self.packs)}

    def minimum_rate_bps(self) -> int:
        return int(self.rule("rp-etr-nta", "etr.nta.minimum_rate").get("rate_bps", 1500))

    def revenue_threshold_kobo(self) -> int:
        return int(self.rule("rp-etr-scope", "etr.scope.revenue_threshold")
                   .get("threshold_ngn_kobo", 12_000_000_000_000_000))

    def excluded_entity_types(self) -> list[str]:
        return list(self.rule("rp-etr-scope", "etr.scope.exclusions")
                    .get("excluded_entity_types", ["governmental", "non_profit", "pension_fund"]))

    def sbie_table(self) -> dict[str, dict[str, int]]:
        return dict(self.rule("rp-globe-oecd", "globe.sbie.transition").get("table", {}))

    def sbie_bps(self, fiscal_year: int) -> tuple[int, int]:
        table = self.sbie_table()
        row = table.get(str(fiscal_year)) or table.get(str(min(max(fiscal_year, 2024), 2033)))
        if not row:
            return 980, 780
        return int(row.get("payroll_bps", 980)), int(row.get("assets_bps", 780))

    def qdmtt_jurisdictions(self) -> list[str]:
        return list(self.rule("rp-etr-nta", "etr.nta.qdmtt").get("qdmtt_jurisdictions", ["NG"]))

    def pope_threshold_bps(self) -> int:
        return int(self.rule("rp-globe-oecd", "globe.iir.ordering").get("pope_threshold_bps", 8000))

    def gir_sections(self) -> list[str]:
        return list(self.rule("rp-gir-schema", "gir.required_sections")
                    .get("sections", ["mne_group", "constituent_entities"]))


def load_packset() -> PackSet:
    ps = PackSet(os.environ.get("RP_REGISTRY_URL", ""))
    ps.load_all()
    return ps
