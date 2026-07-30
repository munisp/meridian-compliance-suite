"""rp-* rule-pack loading and evaluation (SPEC 1.4), mirroring the Go
packages/shared/rulepack semantics. Fallback chain:
rules-engine/rp-registry API (env) -> RP_PACKS_DIR -> repo embedded packs.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import httpx
import yaml

_OPS = ("__exists", "__lte", "__gte", "__in", "__ne", "__lt", "__gt")


@dataclass
class Pack:
    id: str
    version: str
    effective_from: str = ""
    effective_to: str | None = None
    status: str = "published"
    subject_to_regazette: bool = True
    provenance: dict = field(default_factory=dict)
    signed: dict = field(default_factory=dict)
    rules: list[dict] = field(default_factory=list)
    raw: dict = field(default_factory=dict)

    @property
    def ref(self) -> str:
        return f"{self.id}@{self.version}"

    @classmethod
    def from_yaml(cls, text: str) -> "Pack":
        doc = yaml.safe_load(text)
        return cls(
            id=doc["id"], version=str(doc["version"]),
            effective_from=str(doc.get("effective_from", "")),
            effective_to=(None if doc.get("effective_to") in (None, "null")
                          else str(doc.get("effective_to"))),
            status=doc.get("status", "published"),
            subject_to_regazette=bool(doc.get("subject_to_regazette", True)),
            provenance=doc.get("provenance") or {},
            signed=doc.get("signed") or {},
            rules=doc.get("rules") or [],
            raw=doc,
        )


def _repo_packs_dir() -> Path | None:
    """Locate packages/shared/rulepack (contains packs/<id>/<version>.yaml)."""
    here = Path(__file__).resolve()
    for parent in here.parents:
        cand = parent / "packages" / "shared" / "rulepack"
        if (cand / "packs").is_dir():
            return cand
    return None


def _match_cond(key: str, want: Any, ctx: dict) -> tuple[bool, str]:
    base, op = key, "eq"
    for suffix in _OPS:
        if key.endswith(suffix):
            base, op = key[: -len(suffix)], suffix[2:]
            break
    got = ctx.get(base)
    exists = base in ctx
    if op == "exists":
        return (exists == bool(want)), f"{base} exists={exists}"
    if op == "in":
        ok = any(str(item) == str(got) for item in (want or []))
        return ok, "" if ok else f"{base}={got} not in {want}"
    if op == "eq":
        if str(got) == str(want):
            return True, ""
        try:
            if float(got) == float(want):
                return True, ""
        except (TypeError, ValueError):
            pass
        return False, f"{base}={got} != {want}"
    if op == "ne":
        ok, _ = _match_cond(base, want, ctx)
        return (not ok), f"{base} ne {want}"
    try:
        gf, wf = float(got), float(want)
    except (TypeError, ValueError):
        return False, f"{base} not numeric"
    ok = {"lt": gf < wf, "lte": gf <= wf, "gt": gf > wf, "gte": gf >= wf}[op]
    return ok, "" if ok else f"{base}={gf} fails {op} {wf}"


def evaluate(pack: Pack, ctx: dict) -> dict:
    """Evaluate a pack over a context -> {pack, decision, trace}."""
    decision: dict[str, Any] = {}
    trace: list[dict] = []
    for rule in pack.rules:
        when = rule.get("when") or {}
        matched, why = True, ""
        for k, want in when.items():
            ok, why = _match_cond(k, want, ctx)
            if not ok:
                matched = False
                break
        trace.append({"rule_id": rule.get("id"), "matched": matched,
                      "reason": "" if matched else why})
        if matched:
            decision.update(rule.get("then") or {})
    return {"pack": pack.ref, "decision": decision, "trace": trace}


class PackRegistry:
    """Loads packs with the fallback chain: rp-registry HTTP API ->
    RP_PACKS_DIR -> repo-embedded copies."""

    def __init__(self, registry_url: str | None = None,
                 packs_dir: str | None = None) -> None:
        self.registry_url = (registry_url
                             or os.environ.get("RP_REGISTRY_URL", "")).rstrip("/")
        self.packs_dir = packs_dir or os.environ.get("RP_PACKS_DIR")
        self._cache: dict[tuple[str, str], Pack] = {}

    def load(self, pack_id: str, version: str = "") -> Pack:
        key = (pack_id, version)
        if key in self._cache:
            return self._cache[key]
        pack = self._load_uncached(pack_id, version)
        self._cache[key] = pack
        return pack

    def _load_uncached(self, pack_id: str, version: str) -> Pack:
        if self.registry_url:
            try:
                if version:
                    url = f"{self.registry_url}/v1/packs/{pack_id}/{version}"
                else:
                    url = f"{self.registry_url}/v1/packs/{pack_id}/latest"
                resp = httpx.get(url, timeout=3.0)
                if resp.status_code == 200:
                    doc = resp.json()
                    if "yaml" in doc:
                        return Pack.from_yaml(doc["yaml"])
                    return Pack.from_yaml(yaml.safe_dump(doc))
            except Exception:
                pass  # fall through to local copies
        for base in ([Path(self.packs_dir)] if self.packs_dir else []) + \
                    ([_repo_packs_dir()] if _repo_packs_dir() else []):
            if base is None:
                continue
            pack_dir = base / "packs" / pack_id
            if not pack_dir.is_dir():
                continue
            if version:
                cand = pack_dir / f"{version}.yaml"
                if cand.is_file():
                    return Pack.from_yaml(cand.read_text())
            else:
                versions = sorted(pack_dir.glob("*.yaml"))
                if versions:
                    return Pack.from_yaml(versions[-1].read_text())
        raise FileNotFoundError(f"rule pack {pack_id}@{version or 'latest'} not found")

    def evaluate(self, pack_id: str, ctx: dict, version: str = "") -> dict:
        """Try core rules-engine (RULES_ENGINE_URL) then local evaluation."""
        engine = os.environ.get("RULES_ENGINE_URL", "").rstrip("/")
        if engine:
            try:
                resp = httpx.post(f"{engine}/v1/evaluate", timeout=3.0, json={
                    "pack_id": pack_id, "version": version, "context": ctx})
                if resp.status_code == 200:
                    body = resp.json()
                    return {"pack": f"{pack_id}@{version}",
                            "decision": body.get("decision", {}),
                            "trace": body.get("trace", []),
                            "via": "rules-engine"}
            except Exception:
                pass
        out = evaluate(self.load(pack_id, version), ctx)
        out["via"] = "embedded-pack"
        return out
