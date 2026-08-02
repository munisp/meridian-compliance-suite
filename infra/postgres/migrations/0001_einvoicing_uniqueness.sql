-- 0001_einvoicing_uniqueness.sql
-- DB-enforced uniqueness for the einvoicing doc store (audit P0): IRN,
-- tenant-scoped idempotency key, and tenant-scoped (supplier_tin,
-- invoice_number) were previously enforced only in Go maps, which breaks
-- in multi-instance deployments. Partial unique indexes on the JSONB
-- payload keep the doc-table shape while giving Postgres enforcement;
-- rows without the fields are not indexed (NULL/empty excluded).
-- All statements are idempotent.

CREATE UNIQUE INDEX IF NOT EXISTS einvoicing_irn_ux
  ON einvoicing_docs ((doc->>'irn'))
  WHERE collection = 'invoices'
    AND doc->>'irn' IS NOT NULL AND doc->>'irn' <> '';

CREATE UNIQUE INDEX IF NOT EXISTS einvoicing_idem_ux
  ON einvoicing_docs ((doc->>'tenant_id'), (doc->>'idempotency_key'))
  WHERE collection = 'invoices'
    AND doc->>'idempotency_key' IS NOT NULL AND doc->>'idempotency_key' <> '';

CREATE UNIQUE INDEX IF NOT EXISTS einvoicing_suppnum_ux
  ON einvoicing_docs ((doc->>'tenant_id'),
                      (doc#>>'{supplier,tin}'),
                      (doc->>'invoice_number'))
  WHERE collection = 'invoices'
    AND doc#>>'{supplier,tin}' IS NOT NULL AND doc#>>'{supplier,tin}' <> ''
    AND doc->>'invoice_number' IS NOT NULL AND doc->>'invoice_number' <> '';
