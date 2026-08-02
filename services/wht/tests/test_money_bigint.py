"""Money-column width tests (schema audit P1): kobo columns are BIGINT, so
values above the int4 ceiling (₦21,474,836.47 = 2_147_483_647 kobo)
round-trip without overflow, and wht_credits carries tenant_id."""
from sqlalchemy import BigInteger, inspect

from app import db

INT4_CEILING_KOBO = 2_147_483_647          # ₦21,474,836.47
BIG = 500_000_000_00                        # ₦500,000,000.00


def test_kobo_columns_are_bigint():
    for table, col in ((db.Deduction, "amount_kobo"),
                       (db.Deduction, "wht_kobo"),
                       (db.Credit, "credit_kobo")):
        assert isinstance(table.__table__.c[col].type, BigInteger), \
            f"{table.__tablename__}.{col} must be BigInteger"


def test_credit_has_tenant_id():
    assert "tenant_id" in db.Credit.__table__.c


def test_large_amounts_round_trip():
    sess = db.session()
    d = db.Deduction(id="ded-big-1", tenant_id="t1", vendor_tin="12345678-0001",
                     payment_type="contract", beneficiary="V",
                     amount_kobo=BIG, rate_bps=500, wht_kobo=BIG // 20,
                     outcome="deducted", period="2026-01")
    sess.add(d)
    sess.commit()
    got = sess.get(db.Deduction, "ded-big-1")
    assert got.amount_kobo == BIG and got.wht_kobo == BIG // 20
    assert got.amount_kobo > INT4_CEILING_KOBO


def test_large_credit_balance_round_trip():
    sess = db.session()
    sess.add(db.Credit(id="cr-big-1", tenant_id="t1", vendor_tin="87654321-0001",
                       credit_kobo=BIG, source="ded-big-1", period="2026-01"))
    sess.add(db.Credit(id="cr-big-2", tenant_id="t1", vendor_tin="87654321-0001",
                       credit_kobo=-INT4_CEILING_KOBO, source="apply"))
    sess.commit()
    bal = db.credit_balance(sess, "87654321-0001")
    assert bal == BIG - INT4_CEILING_KOBO
    rows = db.vendor_credits(sess, "87654321-0001")
    assert {r.id for r in rows} == {"cr-big-1", "cr-big-2"}
    assert all(r.tenant_id == "t1" for r in rows)


def test_indexes_exist():
    idx = {i.name for i in db.Deduction.__table__.indexes}
    assert "ix_wht_ded_vendor_period" in idx
    assert "ix_wht_ded_remit" in idx
    # indexes are actually created on the live engine
    eng_idx = {i["name"] for i in inspect(db.engine()).get_indexes("wht_deductions")}
    assert "ix_wht_ded_vendor_period" in eng_idx
