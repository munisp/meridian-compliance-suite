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
        # Deterministic batch id (F3): a retried run over the same deduction
        # set regenerates the SAME batch — credits, files and remit_batch
        # markers all key off it, so retries converge instead of forking.
        import hashlib
        ded_key = ",".join(sorted(d["id"] for d in state["deductions"]))
        state["batch_id"] = (
            f"remit-{(period or 'all')}-"
            f"{hashlib.sha256(ded_key.encode()).hexdigest()[:8]}")
        return f"{len(vendors)} vendors, total {total} kobo"

    def generate_files():
        per = period or (state["deductions"][0]["period"]
                         if state["deductions"] else "")
        state["csv"] = remit.remittance_csv(state["batch_id"], state["deductions"])
        state["xml"] = remit.remittance_xml(state["batch_id"],
                                            state["deductions"], per)
        return f"CSV {len(state['csv'])}B, XML {len(state['xml'])}B"

    def post_credits_and_mark_remitted():
        """F3: vendor credits + remitted flags in ONE database transaction
        (audit Flow 3: they were separate transactions, and credit ids were
        random — a retry after a mid-run crash double-posted credits).

        Idempotency: credit id is deterministic per (batch, deduction) and
        existing credits are skipped (dedup store = the credits table
        itself). A crash anywhere before the commit rolls the whole unit
        back; a retry re-runs the unit and converges to exactly one credit
        per deduction."""
        posted = skipped = 0
        with db.session() as sess:
            for d in state["deductions"]:
                cid = f"cr-{state['batch_id']}-{d['id']}"
                # dedup: deterministic credit id per run AND a source-index
                # check (a credit already posted for this deduction by ANY
                # earlier run/crash-orphaned batch is never reposted)
                from sqlalchemy import select as _select
                existing = sess.get(db.Credit, cid) or sess.execute(
                    _select(db.Credit).where(db.Credit.source == d["id"])).scalars().first()
                if existing is None:
                    sess.add(db.Credit(
                        id=cid,
                        vendor_tin=d["vendor_tin"], credit_kobo=d["wht_kobo"],
                        source=d["id"], period=d["period"],
                        note=f"WHT credit from {state['batch_id']}",
                        created_at=db.now()))
                    posted += 1
                else:
                    skipped += 1  # replay: already posted by an earlier attempt
                row = sess.get(db.Deduction, d["id"])
                if row is not None:
                    row.remitted = True
                    row.remit_batch = state["batch_id"]
            sess.commit()  # single atomic commit: credits + remitted flags
        state["credits_posted"] = posted
        state["credits_deduped"] = skipped
        return f"posted {posted} vendor credits ({skipped} deduped), deductions marked remitted"

    def reconcile():
        """Post-condition (audit Flow 3e): Σ(credits for this batch) must
        equal Σ(deductions marked remitted in this batch)."""
        with db.session() as sess:
            from sqlalchemy import func, select
            # Σ credits for THIS deduction set (by source id — dedup-safe)
            # must equal Σ deductions marked remitted in this run.
            credits_total = int(sess.execute(
                select(func.coalesce(func.sum(db.Credit.credit_kobo), 0))
                .where(db.Credit.source.in_([d["id"] for d in state["deductions"]]))).scalar_one())
        if credits_total != state["total_kobo"]:
            raise RuntimeError(
                f"recon break: credits {credits_total} != deductions {state['total_kobo']}")
        return f"reconciled: credits == deductions == {credits_total} kobo"

    try:
        _retry(run, "collect", collect)
        _retry(run, "aggregate", aggregate)
        _retry(run, "generate-files", generate_files)
        _retry(run, "post-credits-and-mark-remitted", post_credits_and_mark_remitted)
        _retry(run, "reconcile", reconcile)
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
