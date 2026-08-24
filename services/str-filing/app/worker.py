"""STR filing retry worker: exponential backoff + dead-letter.

State machine (mirrors the Odoo nrs.submission.log cron):

    pending -> submitting -> filed          (success, terminal)
                          -> failed         (retryable error; backoff)
    failed  -> submitting -> ...            (retried when next_retry_at due)
             -> dlq                         (attempts exhausted or 4xx)
    dlq     -> pending                      (manual requeue, RBAC-gated)
    submitting -> failed                    (B3 #9 recovery sweep: worker
                                             crashed mid-submit; resubmits
                                             under the same idempotency key)

A row is never deleted and never silently dropped: an NFIU outage only
moves rows pending<->failed until max_attempts, then to dlq where they
await manual requeue — no loss (waveC C4/C7).
"""
from __future__ import annotations

import logging
import os
import threading
import time
from datetime import timedelta

from prometheus_client import CollectorRegistry, Counter, Gauge

from . import db
from .audit import AuditTrail
from .nfiu import NFIUClient, NFIURejectedError, NFIUUnavailableError

log = logging.getLogger("str-filing.worker")


class Metrics:
    def __init__(self, registry: CollectorRegistry | None = None):
        self.registry = registry or CollectorRegistry()
        self.dlq_depth = Gauge("str_dlq_depth",
                               "STR filings currently dead-lettered",
                               ["tenant_id"], registry=self.registry)
        self.submission_errors = Counter(
            "str_submission_errors_total", "NFIU submission errors",
            ["tenant_id", "kind"], registry=self.registry)
        self.filed_total = Counter("str_filed_total",
                                   "STRs successfully filed with NFIU",
                                   ["tenant_id"], registry=self.registry)


def backoff_seconds(attempts: int) -> float:
    base = float(os.environ.get("STR_RETRY_BASE_SECONDS", "30"))
    cap = float(os.environ.get("STR_RETRY_MAX_BACKOFF_SECONDS", "3600"))
    return min(cap, base * (2 ** max(0, attempts - 1)))


class FilingWorker:
    def __init__(self, session_factory, adapter: NFIUClient,
                 audit: AuditTrail, metrics: Metrics):
        self.sessions = session_factory
        self.adapter = adapter
        self.audit = audit
        self.metrics = metrics

    # -- internals ---------------------------------------------------------

    def _transition(self, s, rec: db.STRFiling, new_status: str, *,
                    actor: str, detail: str = ""):
        old = rec.status
        rec.status = new_status
        rec.updated_at = db.utcnow()
        s.add(rec)
        s.flush()
        try:
            self.audit.record(actor=actor, str_id=rec.id,
                              tenant_id=rec.tenant_id, old_status=old,
                              new_status=new_status,
                              str_hash=rec.payload_hash, detail=detail)
        except Exception:  # audit must never lose the queue transition
            log.exception("audit record failed for %s (%s->%s)",
                          rec.id, old, new_status)

    def refresh_dlq_depth(self):
        with self.sessions() as s:
            rows = (s.query(db.STRFiling.tenant_id)
                    .filter(db.STRFiling.status == db.STATUS_DLQ).all())
            counts: dict[str, int] = {}
            for (tenant,) in rows:
                counts[tenant] = counts.get(tenant, 0) + 1
            for tenant, n in counts.items():
                self.metrics.dlq_depth.labels(tenant_id=tenant).set(n)
            return counts

    # -- public API --------------------------------------------------------

    def recover_submitting(self, *, grace_seconds: float | None = None) -> int:
        """B3 #9: resume filings stranded in ``submitting`` — the worker
        crashed mid-submit after the state transition but before the
        outcome was recorded, leaving the row in a status no scanner
        selects (precedent: einvoicing nrs_resume.go interrupted-flow
        recovery). The NFIU outcome is unknowable locally, so the row is
        requeued as ``failed`` with ``next_retry_at=now``; ``process_due``
        then resubmits under the SAME idempotency key, which NFIU dedupes
        server-side — the STR is filed at most once. Exactly-once per
        sweep: each stranded row transitions out of ``submitting`` once.
        A grace window (STR_SUBMITTING_GRACE_SECONDS, default 60s) keeps
        rows an in-flight submit is still working on from being swept.
        Returns the number of rows recovered."""
        grace = (float(os.environ.get("STR_SUBMITTING_GRACE_SECONDS", "60"))
                 if grace_seconds is None else grace_seconds)
        cutoff = db.utcnow() - timedelta(seconds=grace)
        with self.sessions() as s:
            stuck = (s.query(db.STRFiling)
                     .filter(db.STRFiling.status == db.STATUS_SUBMITTING,
                             db.STRFiling.updated_at <= cutoff)
                     .order_by(db.STRFiling.created_at).all())
            for rec in stuck:
                rec.next_retry_at = db.utcnow()
                self._transition(
                    s, rec, db.STATUS_FAILED, actor="str-worker",
                    detail="recovery: stranded in submitting (crash "
                           "mid-submit); resubmit under same idempotency key")
            s.commit()
            n = len(stuck)
        if n:
            log.warning("recovered %d STR filing(s) stranded in "
                        "submitting", n)
        return n

    def process_due(self, limit: int = 100) -> int:
        """Attempt every due filing once. Returns the number processed."""
        now = db.utcnow()
        processed = 0
        with self.sessions() as s:
            due = (s.query(db.STRFiling)
                   .filter(db.STRFiling.status.in_(
                       (db.STATUS_PENDING, db.STATUS_FAILED)))
                   .filter((db.STRFiling.next_retry_at.is_(None))
                           | (db.STRFiling.next_retry_at <= now))
                   .order_by(db.STRFiling.created_at).limit(limit).all())
            ids = [r.id for r in due]
        for rid in ids:
            with self.sessions() as s:
                rec = s.get(db.STRFiling, rid)
                if rec is None or rec.status not in (db.STATUS_PENDING,
                                                     db.STATUS_FAILED):
                    continue
                self._transition(s, rec, db.STATUS_SUBMITTING,
                                 actor="str-worker", detail="submit attempt")
                try:
                    ref = self.adapter.submit(
                        str_id=rec.id, tenant_id=rec.tenant_id,
                        report_type=rec.report_type, payload=rec.payload,
                        idempotency_key=rec.idempotency_key)
                except NFIUUnavailableError as exc:
                    rec.attempts += 1
                    rec.last_error = str(exc)[:500]
                    self.metrics.submission_errors.labels(
                        tenant_id=rec.tenant_id, kind="unavailable").inc()
                    if rec.attempts >= rec.max_attempts:
                        self._transition(s, rec, db.STATUS_DLQ,
                                         actor="str-worker",
                                         detail=f"attempts exhausted: {exc}")
                    else:
                        rec.next_retry_at = db.utcnow() + timedelta(
                            seconds=backoff_seconds(rec.attempts))
                        self._transition(s, rec, db.STATUS_FAILED,
                                         actor="str-worker",
                                         detail=f"retryable error: {exc}")
                except NFIURejectedError as exc:
                    rec.attempts += 1
                    rec.last_error = str(exc)[:500]
                    self.metrics.submission_errors.labels(
                        tenant_id=rec.tenant_id, kind="rejected").inc()
                    self._transition(s, rec, db.STATUS_DLQ,
                                     actor="str-worker",
                                     detail=f"permanent rejection: {exc}")
                else:
                    rec.nfiu_reference = ref or ""
                    rec.filed_at = db.utcnow()
                    rec.last_error = ""
                    self._transition(s, rec, db.STATUS_FILED,
                                     actor="str-worker",
                                     detail=f"nfiu ref {rec.nfiu_reference}")
                    self.metrics.filed_total.labels(
                        tenant_id=rec.tenant_id).inc()
                s.commit()
                processed += 1
        self.refresh_dlq_depth()
        return processed

    def requeue(self, str_id: str, *, actor: str) -> db.STRFiling | None:
        """Manual dlq->pending requeue (Odoo action_retry_now equivalent)."""
        with self.sessions() as s:
            rec = s.get(db.STRFiling, str_id)
            if rec is None:
                return None
            if rec.status != db.STATUS_DLQ:
                raise ValueError(
                    f"only dlq filings can be requeued (status={rec.status})")
            rec.attempts = 0
            rec.last_error = ""
            rec.next_retry_at = db.utcnow()
            self._transition(s, rec, db.STATUS_PENDING, actor=actor,
                             detail="manual requeue")
            s.commit()
            self.refresh_dlq_depth()
            return rec

    # -- background loop ---------------------------------------------------

    def run_forever(self, stop: threading.Event, interval: float = 5.0):
        log.info("str filing worker started (interval=%ss)", interval)
        while not stop.is_set():
            try:
                self.recover_submitting()  # B3 #9: boot/periodic sweep
            except Exception:
                log.exception("submitting recovery sweep failed")
            try:
                self.process_due()
            except Exception:
                log.exception("worker iteration failed")
            try:
                with self.sessions() as s:
                    db.purge_expired_idempotency(s)
            except Exception:
                log.exception("idempotency purge failed")
            stop.wait(interval)


def start_background(worker: FilingWorker) -> threading.Event:
    stop = threading.Event()
    interval = float(os.environ.get("STR_WORKER_INTERVAL_SECONDS", "5"))
    t = threading.Thread(target=worker.run_forever, args=(stop, interval),
                         daemon=True, name="str-filing-worker")
    t.start()
    return stop
