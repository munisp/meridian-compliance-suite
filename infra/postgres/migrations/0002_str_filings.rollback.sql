-- 0002_str_filings.rollback.sql — companion rollback for 0002_str_filings.sql.
--
-- ROLLBACK POLICY: forward-fix preferred. The str_filings table carries live
-- STR filing queue state (idempotency keys, NFIU references, retry/DLQ
-- cursors); a DROP TABLE discards in-flight filings and breaks idempotent
-- replay of NFIU submissions, so rollback is only safe when the table is
-- EMPTY (or the deployment never filed). The guard below refuses to drop a
-- non-empty table — fix forward instead (new migration correcting the
-- defect) once filings exist.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM str_filings) THEN
        RAISE EXCEPTION '0002 rollback refused: str_filings is not empty — fix forward with a new migration';
    END IF;
END $$;
DROP TABLE IF EXISTS str_filings;
-- After rollback, delete the 0002 row from schema_migrations so a later
-- boot can re-apply it:
--   DELETE FROM schema_migrations WHERE version = 2;
