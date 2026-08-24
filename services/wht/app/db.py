"""WHT credit ledger (SPEC 3 T7): SQLAlchemy; SQLite fallback in dev,
Postgres when DATABASE_URL is set. Integer kobo only."""

from __future__ import annotations

import os
import time

from sqlalchemy import (BigInteger, Boolean, Column, Index, Integer, String,
                        create_engine, inspect, select, func, text)
from sqlalchemy.orm import DeclarativeBase, Session

DATA_DIR = os.environ.get("DATA_DIR", "/tmp/meridian-wht")
DATABASE_URL = os.environ.get(
    "DATABASE_URL", f"sqlite:///{os.path.join(DATA_DIR, 'wht.db')}")


class Base(DeclarativeBase):
    pass


class Deduction(Base):
    __tablename__ = "wht_deductions"
    id = Column(String, primary_key=True)
    tenant_id = Column(String, default="")
    vendor_tin = Column(String, index=True)
    vendor_name = Column(String, default="")
    payment_type = Column(String)
    beneficiary = Column(String)
    amount_kobo = Column(BigInteger)      # int8: int4 overflows > ₦21,474,836.47
    rate_bps = Column(Integer)
    wht_kobo = Column(BigInteger)
    outcome = Column(String)
    deduction_trigger = Column(String, default="")
    deduction_date = Column(String, default="")
    period = Column(String, index=True)   # YYYY-MM of deduction_date
    remitted = Column(Boolean, default=False)
    remit_batch = Column(String, default="")
    # B3 #20: idempotency bindings are payload-bound — replaying an
    # idempotency key with a different request payload is a conflict, not
    # a silent replay of the original deduction.
    payload_hash = Column(String, default="")

    __table_args__ = (
        Index("ix_wht_ded_vendor_period", "vendor_tin", "period"),
        # remittance batch query: unremitted deductions only (partial on PG)
        Index("ix_wht_ded_remit", "remitted",
              postgresql_where=text("NOT remitted")),
    )


class Credit(Base):
    __tablename__ = "wht_credits"
    id = Column(String, primary_key=True)
    tenant_id = Column(String, default="")  # backfill: '' = pre-tenant rows
    vendor_tin = Column(String, index=True)
    credit_kobo = Column(BigInteger)      # positive = credit, negative = applied
    source = Column(String, default="")   # deduction id / adjustment ref
    period = Column(String, default="")
    note = Column(String, default="")
    created_at = Column(String, default="")


_engine = None


def _migrate(eng) -> None:
    """Idempotent in-place upgrades for pre-existing databases (create_all
    only creates missing tables/indexes, it never alters columns)."""
    insp = inspect(eng)
    with eng.begin() as c:
        cols = {col["name"] for col in insp.get_columns("wht_credits")}
        if "tenant_id" not in cols:
            # backfill note: pre-tenant rows get '' (single-tenant default)
            c.execute(text("ALTER TABLE wht_credits ADD COLUMN tenant_id VARCHAR DEFAULT ''"))
        ded_cols = {col["name"] for col in insp.get_columns("wht_deductions")}
        if "payload_hash" not in ded_cols:
            # B3 #20 backfill: pre-existing rows get '' (unbound legacy)
            c.execute(text("ALTER TABLE wht_deductions ADD COLUMN payload_hash VARCHAR DEFAULT ''"))
        if eng.dialect.name == "postgresql":
            # int4 -> int8 for kobo columns (overflow above ₦21,474,836.47)
            for table, col in (("wht_deductions", "amount_kobo"),
                               ("wht_deductions", "wht_kobo"),
                               ("wht_credits", "credit_kobo")):
                c.execute(text(f"ALTER TABLE {table} ALTER COLUMN {col} TYPE BIGINT"))


def engine():
    global _engine
    if _engine is None:
        os.makedirs(DATA_DIR, exist_ok=True)
        _engine = create_engine(DATABASE_URL)
        Base.metadata.create_all(_engine)
        _migrate(_engine)
    return _engine


def session() -> Session:
    return Session(engine())


def acquire_credit_lock(sess: Session, tenant_id: str, vendor_tin: str) -> None:
    """Serialize credit check+insert per credit account on Postgres
    (B3 #10, R3 verifier): on READ COMMITTED two concurrent
    INSERT ... SELECT ... WHERE balance >= need statements each snapshot
    BEFORE the other commits, so both insert and the ledger overdraws.
    pg_advisory_xact_lock serializes the whole check+insert transaction
    per (tenant, vendor) account key and is released automatically at
    commit/rollback. On SQLite this is a no-op: the conditional INSERT is
    already atomic there (SQLite serializes writes database-wide)."""
    if sess.get_bind().dialect.name == "postgresql":
        sess.execute(
            text("SELECT pg_advisory_xact_lock(hashtext(:k))"),
            {"k": f"wht-credit:{tenant_id}:{vendor_tin}"})


def now() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def credit_balance(sess: Session, vendor_tin: str) -> int:
    total = sess.execute(
        select(func.coalesce(func.sum(Credit.credit_kobo), 0))
        .where(Credit.vendor_tin == vendor_tin)).scalar_one()
    return int(total)


def vendor_credits(sess: Session, vendor_tin: str) -> list[Credit]:
    return list(sess.execute(
        select(Credit).where(Credit.vendor_tin == vendor_tin)
        .order_by(Credit.created_at)).scalars())
