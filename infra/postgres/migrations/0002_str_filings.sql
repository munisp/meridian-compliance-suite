-- str-filing: durable STR filing queue (waveC C4/C7). Service DDL is
-- SQLAlchemy create_all (idempotent); this migration carries the same
-- table for the platform pgmigrate runner convention.
CREATE TABLE IF NOT EXISTS str_filings (
    id VARCHAR(40) PRIMARY KEY,
    tenant_id VARCHAR(64) NOT NULL DEFAULT '',
    idempotency_key VARCHAR(128) NOT NULL,
    subject_ref VARCHAR(128) NOT NULL DEFAULT '',
    report_type VARCHAR(32) NOT NULL DEFAULT 'STR',
    payload TEXT NOT NULL DEFAULT '{}',
    payload_hash VARCHAR(64) NOT NULL DEFAULT '',
    status VARCHAR(16) NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 5,
    last_error TEXT NOT NULL DEFAULT '',
    next_retry_at TIMESTAMPTZ,
    nfiu_reference VARCHAR(128) NOT NULL DEFAULT '',
    created_by VARCHAR(128) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    filed_at TIMESTAMPTZ,
    CONSTRAINT uq_str_tenant_idem UNIQUE (tenant_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS ix_str_filings_status ON str_filings(status);
CREATE INDEX IF NOT EXISTS ix_str_filings_tenant ON str_filings(tenant_id);
