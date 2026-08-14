-- 0001_einvoicing_uniqueness.rollback.sql — companion rollback for
-- 0001_einvoicing_uniqueness.sql.
-- Safe to roll back: these are partial UNIQUE INDEXES (no schema change, no
-- data rewrite). Dropping them only removes DB-level duplicate enforcement;
-- the Go application-level dedup checks remain, so a rollback degrades
-- multi-instance safety but loses no data. Idempotent.
DROP INDEX IF EXISTS einvoicing_irn_ux;
DROP INDEX IF EXISTS einvoicing_idem_ux;
DROP INDEX IF EXISTS einvoicing_suppnum_ux;
-- After rollback, delete the 0001 row from schema_migrations so a later
-- boot can re-apply it:
--   DELETE FROM schema_migrations WHERE version = 1;
