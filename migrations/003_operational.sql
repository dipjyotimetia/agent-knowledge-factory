-- Hash-chained audit, retrieval dates, and redacted query logs.
-- Snapshot bodies stay in Postgres. They are not written to the knowledge Git repo.

ALTER TABLE audit_events ADD COLUMN prev_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_events ADD COLUMN event_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_events ADD COLUMN chain_seq BIGINT NOT NULL DEFAULT 0;

ALTER TABLE knowledge_items ADD COLUMN last_reviewed_at TIMESTAMPTZ;

CREATE TABLE query_logs (
    id UUID PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    query_redacted TEXT NOT NULL,
    result_count INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE query_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE query_logs FORCE ROW LEVEL SECURITY;
CREATE POLICY query_logs_workspace ON query_logs
    USING (workspace_id = current_setting('akf.workspace_id', true))
    WITH CHECK (workspace_id = current_setting('akf.workspace_id', true));
