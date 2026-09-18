"""F4 — Assessment lifecycle + objection workflow (NTAA 2025 ss.34-49).

Lifecycle: issue (additional / best-of-judgment, s.36) -> demand notice
with service metadata (s.40: personal / registered_post / electronic) ->
30-day objection window from service (s.41) -> final-and-conclusive if no
valid objection -> objection decision within 90 days, else deemed upheld
and a TAT referral record is produced (JRBA / rp-procedure-tat). A REJECTED
objection is appealable to the TAT within 30 days (s.42) — final only after
the appeal window lapses or the TAT decides.

s.41 formal validity: objection must state grounds AND the admitted amount;
the admitted amount (<= assessed amount) is payable (partial payment
recorded here). Clocks are injected (`today`) so tests are deterministic;
the 30/90-day windows are statutory constants.

Statutory limitation (audit R4 S1b): an assessment older than its tax
type's limitation period is statute-barred — non-enforceable (payment
demand refused, enforcement transitions blocked) while the record stays
readable.

REAL: lifecycle state machine, deadlines, deemed-upheld default, TAT
referral record. SIM: service of notice is recorded metadata, not actual
dispatch; partial payment is a ledger entry, not a payment rail.
"""
from __future__ import annotations

import itertools
from datetime import date, timedelta

from . import store as _store

OBJECTION_WINDOW_DAYS = 30      # NTAA s.41
DECISION_WINDOW_DAYS = 90       # NTAA s.41: authority must decide; else deemed upheld
APPEAL_WINDOW_DAYS = 30         # TAT appeal window from an adverse objection decision (NTAA s.42 / TAT Act)
SERVICE_CHANNELS = ("personal", "registered_post", "electronic")  # s.40
ASSESSMENT_KINDS = ("additional", "best_of_judgment", "self", "revised")

# Statutory limitation period (years) per tax type for collection/
# enforcement, measured from the END of the assessed period (constant-
# driven per tax type; FIRS Establishment Act s.55 six-year practice).
# An assessment older than this is statute-barred: non-enforceable (no
# payment demand, no enforcement transition) though the record stays
# readable for the taxpayer.
LIMITATION_YEARS = {
    "CIT": 6, "PAYE": 6, "WHT": 6, "CGT": 6,
    "VAT": 6, "DEV_LEVY": 6, "EDT": 6, "STR": 6,
    "DEFAULT": 6,
}


def period_end(period: str) -> date:
    """Last day of a YYYY-MM or YYYY period."""
    y = int(period[:4])
    m = int(period[5:7]) if len(period) >= 7 else 12
    if m == 12:
        return date(y, 12, 31)
    return date(y, m + 1, 1) - timedelta(days=1)


def limitation_expiry(period: str, tax_type: str) -> date:
    """Enforcement limitation expiry: end of period + the tax type's
    limitation years."""
    years = LIMITATION_YEARS.get((tax_type or "").upper(),
                                 LIMITATION_YEARS["DEFAULT"])
    end = period_end(period)
    try:
        return end.replace(year=end.year + years)
    except ValueError:  # Feb 29 -> Feb 28 in the target year
        return end.replace(year=end.year + years, day=28)


def is_statute_barred(assessment: dict, today: date) -> bool:
    return today > limitation_expiry(assessment["period"],
                                     assessment["tax_type"])


_ids = itertools.count(1)


class AssessmentError(ValueError):
    pass


class AssessmentStore:
    """Lifecycle store, durable via app.store.DocStore (Postgres in prod,
    in-memory fallback in dev)."""

    def __init__(self, docs: "_store.DocStore | None" = None):
        self._docs = docs if docs is not None else _store.DocStore()
        # re-seed the shared ID counter past any durably stored ids
        for coll, field, prefix in (("assessments", "assessment_id", "ASM-"),
                                    ("objections", "objection_id", "OBJ-"),
                                    ("tat_referrals", "referral_id", "TAT-"),
                                    ("tat_appeals", "appeal_id", "TAT-")):
            _store.seed_counter(_ids, _store.max_id_suffix(
                self._docs, coll, field, prefix))

    def _put_asm(self, a: dict) -> None:
        self._docs.put("assessments", a["assessment_id"], a)

    def _put_obj(self, obj: dict) -> None:
        self._docs.put("objections", obj["objection_id"], obj)

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
        self._put_asm(rec)
        return rec

    def get(self, assessment_id: str) -> dict | None:
        return self._docs.get("assessments", assessment_id)

    # --- objection (s.41) ------------------------------------------------
    def object(self, assessment_id: str, grounds: str,
               admitted_amount_kobo: int, paid_admitted_kobo: int,
               filed_at: date) -> dict:
        a = self._docs.get("assessments", assessment_id)
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
        self._put_obj(obj)
        a["status"] = "objected"
        a["objection_id"] = obj["objection_id"]
        a["history"].append({"at": filed_at.isoformat(), "event": "objection_filed",
                             "objection_id": obj["objection_id"]})
        if paid:
            a["history"].append({"at": filed_at.isoformat(),
                                 "event": "partial_payment_admitted",
                                 "amount_kobo": paid})
        self._put_asm(a)
        return obj

    def decide(self, objection_id: str, outcome: str, decided_at: date,
               revised_amount_kobo: int | None = None) -> dict:
        """outcome: upheld (taxpayer wins) | partially_upheld | rejected."""
        obj = self._docs.get("objections", objection_id)
        if obj is None:
            raise AssessmentError("unknown objection")
        if obj["status"] != "pending":
            raise AssessmentError(f"objection already {obj['status']}")
        if decided_at > date.fromisoformat(obj["decision_deadline"]):
            raise AssessmentError("decision out of time; objection is deemed upheld")
        if outcome not in ("upheld", "partially_upheld", "rejected"):
            raise AssessmentError(f"unknown outcome {outcome!r}")
        a = self._docs.get("assessments", obj["assessment_id"])
        if outcome == "partially_upheld":
            if revised_amount_kobo is None or not (0 <= int(revised_amount_kobo) < a["amount_kobo"]):
                raise AssessmentError("partially_upheld requires a lower revised amount")
            a["amount_kobo"] = int(revised_amount_kobo)
        elif outcome == "upheld":
            a["amount_kobo"] = obj["admitted_amount_kobo"]
        obj["status"] = outcome
        obj["decided_at"] = decided_at.isoformat()
        if outcome == "rejected":
            # TAT window (audit R4 S1b#11): a REJECTED objection is NOT
            # final — the taxpayer has a 30-day statutory appeal window to
            # the Tax Appeal Tribunal (NTAA s.42 / TAT Act). The assessment
            # becomes final-and-conclusive only after the window lapses
            # without an appeal, or on the TAT's own decision.
            a["status"] = "appealable"
            a["appeal_deadline"] = (decided_at + timedelta(days=APPEAL_WINDOW_DAYS)).isoformat()
            a["tat_appeal_id"] = None
        else:
            a["status"] = "final_and_conclusive"
        a["history"].append({"at": decided_at.isoformat(), "event": "objection_decided",
                             "outcome": outcome})
        self._put_obj(obj)
        self._put_asm(a)
        return obj

    # --- TAT appeal (NTAA s.42) ------------------------------------------
    def appeal(self, assessment_id: str, grounds: str, filed_at: date) -> dict:
        """File a TAT appeal against a rejected objection decision (30-day
        window from the decision)."""
        a = self._docs.get("assessments", assessment_id)
        if a is None:
            raise AssessmentError("unknown assessment")
        if a["status"] != "appealable":
            raise AssessmentError(f"assessment is {a['status']}; not appealable")
        if filed_at > date.fromisoformat(a["appeal_deadline"]):
            raise AssessmentError("appeal out of time (30 days from the "
                                  "objection decision, NTAA s.42)")
        appeal = {
            "appeal_id": f"TAT-{next(_ids):06d}",
            "assessment_id": assessment_id,
            "objection_id": a["objection_id"],
            "tin": a["tin"], "tax_type": a["tax_type"],
            "period": a["period"], "grounds": grounds,
            "disputed_amount_kobo": a["amount_kobo"],
            "filed_at": filed_at.isoformat(), "status": "pending",
        }
        self._docs.put("tat_appeals", appeal["appeal_id"], appeal)
        a["status"] = "under_appeal"
        a["tat_appeal_id"] = appeal["appeal_id"]
        a["history"].append({"at": filed_at.isoformat(),
                             "event": "tat_appeal_filed",
                             "appeal_id": appeal["appeal_id"]})
        self._put_asm(a)
        return appeal

    def tat_decision(self, appeal_id: str, outcome: str, decided_at: date,
                     varied_amount_kobo: int | None = None) -> dict:
        """Record the TAT's decision: affirmed | varied | annulled. Only now
        does the assessment become final-and-conclusive."""
        appeal = self._docs.get("tat_appeals", appeal_id)
        if appeal is None:
            raise AssessmentError("unknown appeal")
        if appeal["status"] != "pending":
            raise AssessmentError(f"appeal already {appeal['status']}")
        if outcome not in ("affirmed", "varied", "annulled"):
            raise AssessmentError(f"unknown TAT outcome {outcome!r}")
        a = self._docs.get("assessments", appeal["assessment_id"])
        if outcome == "varied":
            if varied_amount_kobo is None or int(varied_amount_kobo) < 0:
                raise AssessmentError("varied requires a revised amount")
            a["amount_kobo"] = int(varied_amount_kobo)
        elif outcome == "annulled":
            obj = self._docs.get("objections", appeal["objection_id"])
            a["amount_kobo"] = (obj or {}).get("admitted_amount_kobo", 0)
        appeal["status"] = outcome
        appeal["decided_at"] = decided_at.isoformat()
        a["status"] = "final_and_conclusive"
        a["history"].append({"at": decided_at.isoformat(),
                             "event": "tat_decided", "outcome": outcome})
        self._docs.put("tat_appeals", appeal["appeal_id"], appeal)
        self._put_asm(a)
        return appeal

    # --- enforcement --------------------------------------------------
    def demand_payment(self, assessment_id: str, today: date) -> dict:
        """Payment demand on a final assessment (enforcement point). A
        statute-barred assessment is NON-ENFORCEABLE: the lazy limitation
        check marks it statute_barred and refuses the demand; the record
        itself stays readable (audit R4 S1b)."""
        a = self._docs.get("assessments", assessment_id)
        if a is None:
            raise AssessmentError("unknown assessment")
        if a["status"] == "statute_barred":
            raise AssessmentError("assessment is statute-barred; not enforceable")
        if is_statute_barred(a, today):
            a["status"] = "statute_barred"
            a["history"].append({"at": today.isoformat(),
                                 "event": "statute_barred",
                                 "limitation_expired": limitation_expiry(
                                     a["period"], a["tax_type"]).isoformat()})
            self._put_asm(a)
            raise AssessmentError(
                "assessment is statute-barred (limitation expired "
                f"{limitation_expiry(a['period'], a['tax_type']).isoformat()}); "
                "enforcement refused")
        if a["status"] not in ("final_and_conclusive",):
            raise AssessmentError(f"assessment is {a['status']}; not yet enforceable")
        a["history"].append({"at": today.isoformat(),
                             "event": "payment_demanded"})
        self._put_asm(a)
        return {"assessment_id": assessment_id,
                "amount_due_kobo": a["amount_kobo"],
                "limitation_expires": limitation_expiry(
                    a["period"], a["tax_type"]).isoformat()}

    # --- clocks ----------------------------------------------------------
    def tick(self, today: date) -> list[dict]:
        """Advance clocks: lapse open assessments past the objection window
        to final-and-conclusive; deem undecided objections past 90 days
        upheld and emit a TAT referral record. Also: lapse appealable
        assessments past the 30-day TAT window (final by default), and mark
        any assessment whose statutory limitation period has expired
        statute_barred (non-enforceable; record stays readable)."""
        events = []
        for a in self._docs.scan("assessments"):
            if a["status"] in ("final_and_conclusive", "appealable",
                               "under_appeal", "objected", "open") and \
                    is_statute_barred(a, today):
                # Sweeper (audit R4 S1b): time-barred assessments are
                # marked non-enforceable wherever enforcement could begin.
                a["status"] = "statute_barred"
                a["history"].append({"at": today.isoformat(),
                                     "event": "statute_barred",
                                     "limitation_expired": limitation_expiry(
                                         a["period"], a["tax_type"]).isoformat()})
                self._put_asm(a)
                events.append({"assessment_id": a["assessment_id"],
                               "event": "statute_barred"})
                continue
            if a["status"] == "appealable":
                if today > date.fromisoformat(a["appeal_deadline"]):
                    a["status"] = "final_and_conclusive"
                    a["history"].append({"at": today.isoformat(),
                                         "event": "final_and_conclusive_no_appeal"})
                    self._put_asm(a)
                    events.append({"assessment_id": a["assessment_id"],
                                   "event": "final_and_conclusive"})
                continue
            if a["status"] == "open":
                if today > date.fromisoformat(a["demand_notice"]["objection_deadline"]):
                    a["status"] = "final_and_conclusive"
                    a["history"].append({"at": today.isoformat(),
                                         "event": "final_and_conclusive_no_objection"})
                    self._put_asm(a)
                    events.append({"assessment_id": a["assessment_id"],
                                   "event": "final_and_conclusive"})
            elif a["status"] == "objected" and a["objection_id"]:
                obj = self._docs.get("objections", a["objection_id"])
                if obj and obj["status"] == "pending" and today > date.fromisoformat(obj["decision_deadline"]):
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
                    self._docs.put("tat_referrals", referral["referral_id"], referral)
                    a["history"].append({"at": today.isoformat(),
                                         "event": "objection_deemed_upheld",
                                         "tat_referral_id": referral["referral_id"]})
                    self._put_obj(obj)
                    self._put_asm(a)
                    events.append(referral)
        return events

    def tat_referrals(self) -> list[dict]:
        return self._docs.scan("tat_referrals")
