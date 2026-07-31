"""I12 — E-invoice anomaly explainability cards.

For each flagged invoice, render the top contributing rule/model reasons as a
human-readable card. Rule reasons come from the validator trace; model
feature contributions plug into the ML feature-store interface.

REAL: rule-trace -> ranked reasons -> card. SIMULATED: feature-store model
scores (FeatureStore.fetch returns None unless FEATURE_STORE_URL is set).
"""
from __future__ import annotations

import os
from dataclasses import dataclass, field

SEVERITY_RANK = {"fatal": 3, "error": 2, "warn": 1, "info": 0}


@dataclass
class Reason:
    source: str          # rule | model
    code: str
    severity: str
    text: str
    weight: float = 1.0


@dataclass
class Card:
    invoice_id: str
    headline: str
    reasons: list[Reason] = field(default_factory=list)
    model_score: float | None = None

    def as_dict(self) -> dict:
        return {"invoice_id": self.invoice_id, "headline": self.headline,
                "model_score": self.model_score,
                "reasons": [r.__dict__ for r in self.reasons]}


class FeatureStore:
    """ML feature-store hook (SIMULATED without FEATURE_STORE_URL)."""

    def __init__(self, url: str | None = None) -> None:
        self.url = url or os.environ.get("FEATURE_STORE_URL", "")

    def fetch(self, invoice_id: str) -> dict | None:
        if not self.url:
            return None  # SIMULATED: no model scores in dev
        raise NotImplementedError  # pragma: no cover


def build_card(invoice_id: str, trace: list[dict],
               feature_store: FeatureStore | None = None,
               top: int = 5) -> Card:
    """trace: validator trace entries [{rule_id, matched, violation, severity, narrate}]"""
    reasons: list[Reason] = []
    for t in trace:
        if not t.get("matched") or not t.get("violation"):
            continue
        reasons.append(Reason(
            source="rule", code=t.get("rule_id", "?"),
            severity=t.get("severity", "warn"),
            text=t.get("narrate") or t.get("violation") or t.get("rule_id", "")))
    scores = (feature_store or FeatureStore()).fetch(invoice_id)
    model_score = None
    if scores:
        model_score = scores.get("score")
        for feat, contrib in sorted(scores.get("contributions", {}).items(),
                                    key=lambda kv: -abs(kv[1]))[:3]:
            reasons.append(Reason("model", feat, "info",
                                  f"feature {feat} contributed {contrib:+.2f}",
                                  abs(contrib)))
    reasons.sort(key=lambda r: (-SEVERITY_RANK.get(r.severity, 0), -r.weight))
    top_reasons = reasons[:top]
    fatal = any(r.severity == "fatal" for r in top_reasons)
    headline = ("BLOCKED: fatal rule violations" if fatal else
                "FLAGGED: review recommended" if top_reasons else "CLEAN")
    return Card(invoice_id, headline, top_reasons, model_score)
