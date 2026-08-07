"""Durable STR filing queue (Postgres in prod, SQLite for dev/test).

Modelled on the Odoo ``nrs.submission.log`` retry/requeue pattern
(integrations/odoo/meridian_nrs_einvoice/models/nrs_submission_log.py):
one row per STR, a state machine, attempt counter + max_attempts, and a
next-retry timestamp the worker polls.

Selection: STR_DATABASE_URL (else DATABASE_URL) set -> Postgres via
SQLAlchemy/psycopg (profile=prod); unset -> SQLite file under DATA_DIR
(profile=dev) so the service still runs standalone with zero deps, matching
the filings/wht store convention. tenant_id is carried on every row.
"""
from __future__ import annotations

import os
import uuid
from datetime import datetime, timezone

from sqlalchemy import (DateTime, Integer, String, Text, UniqueConstraint,
                        create_engine)
from sqlalchemy.orm import DeclarativeBase, Mapped, mapped_column, sessionmaker

STATUS_PENDING = "pending"
STATUS_SUBMITTING = "submitting"
STATUS_FILED = "filed"
STATUS_FAILED = "failed"
STATUS_DLQ = "dlq"
TERMINAL = (STATUS_FILED, STATUS_DLQ)


def utcnow() -> datetime:
    return datetime.now(timezone.utc).replace(tzinfo=None)


def new_id() -> str:
    return "str-" + uuid.uuid4().hex[:24]


class Base(DeclarativeBase):
    pass


class STRFiling(Base):
    __tablename__ = "str_filings"
    __table_args__ = (
        UniqueConstraint("tenant_id", "idempotency_key",
                         name="uq_str_tenant_idem"),
    )

    id: Mapped[str] = mapped_column(String(40), primary_key=True,
                                    default=new_id)
    tenant_id: Mapped[str] = mapped_column(String(64), index=True, default="")
    idempotency_key: Mapped[str] = mapped_column(String(128), index=True)
    subject_ref: Mapped[str] = mapped_column(String(128), index=True,
                                             default="")
    report_type: Mapped[str] = mapped_column(String(32), default="STR")
    payload: Mapped[str] = mapped_column(Text, default="{}")  # canonical JSON
    payload_hash: Mapped[str] = mapped_column(String(64), default="")
    status: Mapped[str] = mapped_column(String(16), index=True,
                                        default=STATUS_PENDING)
    attempts: Mapped[int] = mapped_column(Integer, default=0)
    max_attempts: Mapped[int] = mapped_column(Integer, default=5)
    last_error: Mapped[str] = mapped_column(Text, default="")
    next_retry_at: Mapped[datetime | None] = mapped_column(DateTime,
                                                           nullable=True)
    nfiu_reference: Mapped[str] = mapped_column(String(128), default="")
    created_by: Mapped[str] = mapped_column(String(128), default="")
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow)
    updated_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow,
                                                 onupdate=utcnow)
    filed_at: Mapped[datetime | None] = mapped_column(DateTime, nullable=True)

    def to_dict(self) -> dict:
        def iso(v):
            return v.isoformat() + "Z" if v else None
        return {
            "id": self.id, "tenant_id": self.tenant_id,
            "idempotency_key": self.idempotency_key,
            "subject_ref": self.subject_ref, "report_type": self.report_type,
            "payload_hash": self.payload_hash, "status": self.status,
            "attempts": self.attempts, "max_attempts": self.max_attempts,
            "last_error": self.last_error,
            "next_retry_at": iso(self.next_retry_at),
            "nfiu_reference": self.nfiu_reference,
            "created_by": self.created_by,
            "created_at": iso(self.created_at),
            "updated_at": iso(self.updated_at),
            "filed_at": iso(self.filed_at),
        }


def database_url() -> str:
    url = os.environ.get("STR_DATABASE_URL") or os.environ.get("DATABASE_URL")
    if url:
        return url
    data_dir = os.environ.get("DATA_DIR", "/tmp/str-filing")
    os.makedirs(data_dir, exist_ok=True)
    return "sqlite:///" + os.path.join(data_dir, "str_filings.db")


def make_engine(url: str | None = None):
    url = url or database_url()
    kwargs = {"pool_pre_ping": True} if not url.startswith("sqlite") else {}
    engine = create_engine(url, **kwargs)
    Base.metadata.create_all(engine)
    return engine


def make_session_factory(engine):
    return sessionmaker(bind=engine, expire_on_commit=False)
