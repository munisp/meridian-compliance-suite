"""wf-wht-remit-schedule (SPEC 3 T7): in-process workflow runner with
Temporal-equivalent semantics (ordered steps, retry with backoff, run
history). Steps: collect -> aggregate -> generate-files -> post-credits ->
mark-remitted -> evidence."""

from __future__ import annotations

import time
import uuid
from dataclasses import dataclass, field

from sqlalchemy import select

from . import db, remit


@dataclass
class StepLog:
    name: str
    attempt: int
    status: str
    detail: str = ""


@dataclass
class WorkflowRun:
    id: str
    name: str
    status: str = "running"
    steps: list[StepLog] = field(default_factory=list)
    result: dict = field(default_factory=dict)
    started_at: str = field(default_factory=db.now)
    finished_at: str = ""


_RUNS: list[WorkflowRun] = []


def runs() -> list[WorkflowRun]:
    return list(_RUNS)


def _retry(run: WorkflowRun, name: str, fn, attempts: int = 3):
    last = None
    for i in range(1, attempts + 1):
        try:
            detail = fn()
            _log(run, name, i, "ok", detail)
            return
        except Exception as exc:  # noqa: BLE001 - activity retry boundary
            last = exc
            _log(run, name, i, "failed", str(exc))
            time.sleep(0.02 * i)
    raise RuntimeError(f"activity {name} failed after {attempts} attempts: {last}")


def _log(run: WorkflowRun, name: str, attempt: int, status: str, detail: str):
    for s in run.steps:
        if s.name == name:
            s.attempt, s.status, s.detail = attempt, status, detail
            return
    run.steps.append(StepLog(name, attempt, status, detail))


def wf_wht_remit_schedule(period: str = "", tenant_id: str = "") -> WorkflowRun:
    """Run the remittance workflow for a period (default: all unremitted)."""
    run = WorkflowRun(id=f"run-wf-wht-remit-schedule-{uuid.uuid4().hex[:8]}",
                      name="wf-wht-remit-schedule")
    state: dict = {}

    def collect():
        with db.session() as sess:
            q = select(db.Deduction).where(db.Deduction.remitted.is_(False))
            if period:
                q = q.where(db.Deduction.period == period)
            if tenant_id:
                q = q.where(db.Deduction.tenant_id == tenant_id)
            rows = list(sess.execute(q).scalars())
            state["deductions"] = [
                {c.name: getattr(r, c.name)
                 for c in db.Deduction.__table__.columns} for r in rows]
        return f"collected {len(state['deductions'])} unremitted deductions"

    def aggregate():
        total = sum(d["wht_kobo"] for d in state["deductions"])
        vendors = {d["vendor_tin"] for d in state["deductions"]}
        state["total_kobo"] = total
        state["batch_id"] = f"remit-{(period or 'all')}-{uuid.uuid4().hex[:8]}"
        return f"{len(vendors)} vendors, total {total} kobo"

    def generate_files():
        per = period or (state["deductions"][0]["period"]
                         if state["deductions"] else "")
        state["csv"] = remit.remittance_csv(state["batch_id"], state["deductions"])
        state["xml"] = remit.remittance_xml(state["batch_id"],
                                            state["deductions"], per)
        return f"CSV {len(state['csv'])}B, XML {len(state['xml'])}B"

    def post_credits():
        with db.session() as sess:
            for d in state["deductions"]:
                sess.add(db.Credit(
                    id=f"cr-{uuid.uuid4().hex[:12]}",
                    vendor_tin=d["vendor_tin"], credit_kobo=d["wht_kobo"],
                    source=d["id"], period=d["period"],
                    note=f"WHT credit from {state['batch_id']}",
                    created_at=db.now()))
            sess.commit()
        return f"posted {len(state['deductions'])} vendor credits"

    def mark_remitted():
        with db.session() as sess:
            for d in state["deductions"]:
                row = sess.get(db.Deduction, d["id"])
                if row is not None:
                    row.remitted = True
                    row.remit_batch = state["batch_id"]
            sess.commit()
        return "deductions marked remitted"

    try:
        _retry(run, "collect", collect)
        _retry(run, "aggregate", aggregate)
        _retry(run, "generate-files", generate_files)
        _retry(run, "post-credits", post_credits)
        _retry(run, "mark-remitted", mark_remitted)
        run.status = "completed"
        run.result = {"batch_id": state["batch_id"],
                      "total_wht_kobo": state["total_kobo"],
                      "deductions": len(state["deductions"]),
                      "csv": state["csv"], "xml": state["xml"]}
    except Exception as exc:  # noqa: BLE001
        run.status = "failed"
        run.result = {"error": str(exc)}
    run.finished_at = db.now()
    _RUNS.append(run)
    return run
