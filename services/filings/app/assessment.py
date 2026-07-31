"""F4 — Assessment lifecycle + objection workflow (NTAA 2025 ss.34-49).

Lifecycle: issue (additional / best-of-judgment, s.36) -> demand notice
with service metadata (s.40: personal / registered_post / electronic) ->
30-day objection window from service (s.41) -> final-and-conclusive if no
valid objection -> objection decision within 90 days, else deemed upheld
and a TAT referral record is produced (JRBA / rp-procedure-tat).

s.41 formal validity: objection must state grounds AND the admitted amount;
the admitted amount (<= assessed amount) is payable (partial payment
recorded here). Clocks are injected (`today`) so tests are deterministic;
the 30/90-day windows are statutory constants.

REAL: lifecycle state machine, deadlines, deemed-upheld default, TAT
referral record. SIM: service of notice is recorded metadata, not actual
dispatch; partial payment is a ledger entry, not a payment rail.
"""
from __future__ import annotations

import itertools
from datetime import date, timedelta

OBJECTION_WINDOW_DAYS = 30      # NTAA s.41
DECISION_WINDOW_DAYS = 90       # NTAA s.41: authority must decide; else deemed upheld
SERVICE_CHANNELS = ("personal", "registered_post", "electronic")  # s.40
ASSESSMENT_KINDS = ("additional", "best_of_judgment", "self", "revised")

_ids = itertools.count(1)


class AssessmentError(ValueError):
    pass


class AssessmentStore:
    """In-memory lifecycle store (insights-style backend; swap for DB)."""

    def __init__(self):
        self._assessments: dict[str, dict] = {}
        self._objections: dict[str, dict] = {}
        self._tat_referrals: list[dict] = []

    # --- issuance -------------------------------------------------------
    def issue(self, tin: str, tax_type: str, period: str, kind: str,
              amount_kobo: int, grounds: str,
              served_via: str, served_at: date) -> dict:
        if kind not in ASSESSMENT_KINDS:
            raise AssessmentError(f"unknown assessment kind {kind!r}")
        if served_via not in SERVICE_CHANNELS:
            raise AssessmentError(f"invalid service channel {served_via!r} (NTAA s.40)")
        if int(amount_kobo) <= 0:
            raise AssessmentError("assessment amount must be positive")
        rec = {
            "assessment_id": f"ASM-{next(_ids):06d}",
            "tin": tin, "tax_type": tax_type.upper(), "period": period,
            "kind": kind, "amount_kobo": int(amount_kobo), "grounds": grounds,
            "demand_notice": {
                "served_via": served_via, "served_at": served_at.isoformat(),
                "objection_deadline": (served_at + timedelta(days=OBJECTION_WINDOW_DAYS)).isoformat(),
            },
            "status": "open",
            "objection_id": None,
            "history": [{"at": served_at.isoformat(), "event": "issued",
                         "kind": kind, "amount_kobo": int(amount_kobo)}],
        }
        self._assessments[rec["assessment_id"]] = rec
        return rec

    def get(self, assessment_id: str) -> dict | None:
        return self._assessments.get(assessment_id)

    # --- objection (s.41) ------------------------------------------------
    def object(self, assessment_id: str, grounds: str,
               admitted_amount_kobo: int, paid_admitted_kobo: int,
               filed_at: date) -> dict:
        a = self._assessments.get(assessment_id)
        if a is None:
            raise AssessmentError("unknown assessment")
        if a["status"] not in ("open",):
            raise AssessmentError(f"assessment is {a['status']}; cannot object")
        deadline = date.fromisoformat(a["demand_notice"]["objection_deadline"])
        if filed_at > deadline:
            raise AssessmentError("objection out of time (30 days from service, s.41)")
        if not grounds or not grounds.strip():
            raise AssessmentError("objection must state grounds (s.41)")
        admitted = int(admitted_amount_kobo)
        if admitted < 0 or admitted > a["amount_kobo"]:
            raise AssessmentError("admitted amount must be 0..assessed amount")
        paid = int(paid_admitted_kobo)
        if paid > admitted:
            raise AssessmentError("payment exceeds admitted amount")
        obj = {
            "objection_id": f"OBJ-{next(_ids):06d}",
            "assessment_id": assessment_id,
            "grounds": grounds,
            "admitted_amount_kobo": admitted,
            "paid_admitted_kobo": paid,
            "disputed_amount_kobo": a["amount_kobo"] - admitted,
            "filed_at": filed_at.isoformat(),
            "decision_deadline": (filed_at + timedelta(days=DECISION_WINDOW_DAYS)).isoformat(),
            "status": "pending",
        }
        self._objections[obj["objection_id"]] = obj
        a["status"] = "objected"
        a["objection_id"] = obj["objection_id"]
        a["history"].append({"at": filed_at.isoformat(), "event": "objection_filed",
                             "objection_id": obj["objection_id"]})
        if paid:
            a["history"].append({"at": filed_at.isoformat(),
                                 "event": "partial_payment_admitted",
                                 "amount_kobo": paid})
        return obj

    def decide(self, objection_id: str, outcome: str, decided_at: date,
               revised_amount_kobo: int | None = None) -> dict:
        """outcome: upheld (taxpayer wins) | partially_upheld | rejected."""
        obj = self._objections.get(objection_id)
        if obj is None:
            raise AssessmentError("unknown objection")
        if obj["status"] != "pending":
            raise AssessmentError(f"objection already {obj['status']}")
        if decided_at > date.fromisoformat(obj["decision_deadline"]):
            raise AssessmentError("decision out of time; objection is deemed upheld")
        if outcome not in ("upheld", "partially_upheld", "rejected"):
            raise AssessmentError(f"unknown outcome {outcome!r}")
        a = self._assessments[obj["assessment_id"]]
        if outcome == "partially_upheld":
            if revised_amount_kobo is None or not (0 <= int(revised_amount_kobo) < a["amount_kobo"]):
                raise AssessmentError("partially_upheld requires a lower revised amount")
            a["amount_kobo"] = int(revised_amount_kobo)
        elif outcome == "upheld":
            a["amount_kobo"] = obj["admitted_amount_kobo"]
        obj["status"] = outcome
        obj["decided_at"] = decided_at.isoformat()
        a["status"] = "final_and_conclusive"
        a["history"].append({"at": decided_at.isoformat(), "event": "objection_decided",
                             "outcome": outcome})
        return obj

    # --- clocks ----------------------------------------------------------
    def tick(self, today: date) -> list[dict]:
        """Advance clocks: lapse open assessments past the objection window
        to final-and-conclusive; deem undecided objections past 90 days
        upheld and emit a TAT referral record."""
        events = []
        for a in self._assessments.values():
            if a["status"] == "open":
                if today > date.fromisoformat(a["demand_notice"]["objection_deadline"]):
                    a["status"] = "final_and_conclusive"
                    a["history"].append({"at": today.isoformat(),
                                         "event": "final_and_conclusive_no_objection"})
                    events.append({"assessment_id": a["assessment_id"],
                                   "event": "final_and_conclusive"})
            elif a["status"] == "objected" and a["objection_id"]:
                obj = self._objections[a["objection_id"]]
                if obj["status"] == "pending" and today > date.fromisoformat(obj["decision_deadline"]):
                    obj["status"] = "deemed_upheld"
                    a["amount_kobo"] = obj["admitted_amount_kobo"]
                    a["status"] = "final_and_conclusive"
                    referral = {
                        "referral_id": f"TAT-{next(_ids):06d}",
                        "assessment_id": a["assessment_id"],
                        "objection_id": obj["objection_id"],
                        "tin": a["tin"], "tax_type": a["tax_type"],
                        "period": a["period"],
                        "basis": "objection deemed upheld (90-day decision lapse, NTAA s.41)",
                        "referred_at": today.isoformat(),
                    }
                    self._tat_referrals.append(referral)
                    a["history"].append({"at": today.isoformat(),
                                         "event": "objection_deemed_upheld",
                                         "tat_referral_id": referral["referral_id"]})
                    events.append(referral)
        return events

    def tat_referrals(self) -> list[dict]:
        return list(self._tat_referrals)
