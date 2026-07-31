"""Runtime citation chain (LCE SPEC §5) — Python mirror of
packages/shared/rulepack/citation.go.

Given a matched rule id + pack version, resolve the statute citation from the
canonical coverage matrix. Reverse index is built from coverage/*.yaml, loaded
read-only; path resolution order:

  1. explicit ``coverage_dir`` argument
  2. env ``LCE_COVERAGE_DIR``
  3. vendored copy at ``packages/shared/rulepack/coverage`` (repo walk)

A missing/unreadable coverage file yields empty ``statute_sections`` — never
an error (SPEC §5.1 rule b). Unknown per-row fields (e.g. ``ctc:`` added by
the registry workstream) are tolerated.

Citation schema (JSON):
  {
    "pack_id": "rp-wht-2024",
    "pack_version": "1.0.0",
    "rule_id": "wht.rate.directors-fees.non-resident",
    "statute": "wht-regs-2024",
    "section_id": "first-schedule.directors-fees",
    "title": "Directors' fees — 15% resident individual / 20% non-resident (final)",
    "citation": "WHT Regs 2024, First Schedule (KPMG rate table — ...)",
    "statute_sections": ["wht-regs-2024:first-schedule.directors-fees"],
    "citation_kind": "secondary",
    "subject_to_regazette": true
  }

[REAL] resolution from the coverage matrix; matrix content citation_kind is
"secondary" until CTC verification (registry workstream owns the CTC feed).
"""

from __future__ import annotations

import os
from pathlib import Path
from typing import Any

import yaml

_resolver_cache: dict[str, "CitationResolver"] = {}


def _repo_coverage_dir() -> Path | None:
    here = Path(__file__).resolve()
    for parent in here.parents:
        cand = (parent / "packages" / "shared" / "rulepack" / "coverage")
        if cand.is_dir():
            return cand
    return None


class CitationResolver:
    """Reverse index: "<pack_id>:<rule_id>" -> coverage section hits."""

    def __init__(self, coverage_dir: str | os.PathLike | None = None) -> None:
        self.index: dict[str, list[dict]] = {}
        if coverage_dir is None:
            coverage_dir = os.environ.get("LCE_COVERAGE_DIR") or _repo_coverage_dir()
        if not coverage_dir:
            return
        base = Path(coverage_dir)
        if not base.is_dir():
            return  # degrade to empty index, never an error
        for path in sorted(base.glob("*.yaml")):
            try:
                doc = yaml.safe_load(path.read_text())
            except Exception:
                continue
            self._add_doc(doc)

    def _add_doc(self, doc: Any) -> None:
        if not isinstance(doc, dict):
            return
        statute = doc.get("statute") or {}
        statute_id = statute.get("id")
        if not statute_id:
            return
        for sec in doc.get("sections") or []:
            for ref in sec.get("implementing_rules") or []:
                key = str(ref).strip()
                if key:
                    self.index.setdefault(key, []).append({
                        "statute": statute, "section": sec})

    def resolve(self, pack_id: str, pack_version: str, rule_id: str,
                rule_citation: str = "") -> dict:
        """Resolve one matched rule to its statute citation (total function)."""
        cit: dict[str, Any] = {
            "pack_id": pack_id,
            "pack_version": pack_version,
            "rule_id": rule_id,
            "citation": rule_citation or "",
            "statute_sections": [],
        }
        hits = sorted(self.index.get(f"{pack_id}:{rule_id}", []),
                      key=lambda h: (h["statute"].get("id", ""),
                                     h["section"].get("section_id", "")))
        if not hits:
            return cit
        first = hits[0]
        statute, section = first["statute"], first["section"]
        cit["statute"] = statute.get("id", "")
        cit["section_id"] = section.get("section_id", "")
        cit["title"] = section.get("title", "")
        if section.get("citation_kind"):
            cit["citation_kind"] = section["citation_kind"]
        cit["subject_to_regazette"] = bool(
            statute.get("subject_to_regazette", False))
        if not cit["citation"]:
            cit["citation"] = ", ".join(
                p for p in (statute.get("title", ""), section.get("title", "")) if p)
        cit["statute_sections"] = [
            f"{h['statute'].get('id', '')}:{h['section'].get('section_id', '')}"
            for h in hits]
        return cit

    def build(self, pack_id: str, pack_version: str, rule_ids: list[str],
              rule_citations: dict[str, str] | None = None) -> list[dict]:
        rule_citations = rule_citations or {}
        return [self.resolve(pack_id, pack_version, rid,
                             rule_citations.get(rid, ""))
                for rid in rule_ids]


def get_resolver(coverage_dir: str | os.PathLike | None = None) -> CitationResolver:
    """Process-wide cached resolver (index built once at first use)."""
    key = str(coverage_dir or os.environ.get("LCE_COVERAGE_DIR") or "<default>")
    if key not in _resolver_cache:
        _resolver_cache[key] = CitationResolver(coverage_dir)
    return _resolver_cache[key]


def build_citations(pack: Any, matched_rule_ids: list[str],
                    coverage_dir: str | os.PathLike | None = None) -> list[dict]:
    """SPEC §5.2 entry point. ``pack`` is a meridian_py.rulepack.Pack (or any
    object with id/version/rules); rule-level ``citation`` text is carried
    through from the pack YAML."""
    pack_id = getattr(pack, "id", "") or ""
    pack_version = str(getattr(pack, "version", "") or "")
    rule_citations = {
        r.get("id"): r.get("citation", "")
        for r in (getattr(pack, "rules", None) or []) if r.get("id")}
    return get_resolver(coverage_dir).build(
        pack_id, pack_version, matched_rule_ids, rule_citations)
