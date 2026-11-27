-- Clear legacy references as opaque values. Temporarily lift FORCE for the
-- table owner within this migration transaction so every workspace is covered.
ALTER TABLE gap_reports NO FORCE ROW LEVEL SECURITY;
ALTER TABLE agent_runs NO FORCE ROW LEVEL SECURITY;
UPDATE gap_reports SET trace_ref = NULL WHERE trace_ref IS NOT NULL;
UPDATE agent_runs SET trace_ref = NULL WHERE trace_ref IS NOT NULL;
ALTER TABLE gap_reports FORCE ROW LEVEL SECURITY;
ALTER TABLE agent_runs FORCE ROW LEVEL SECURITY;

ALTER TABLE gap_reports
    ADD COLUMN trace_actor text,
    ADD COLUMN trace_curator boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT gap_trace_authority CHECK (
        (trace_ref IS NULL AND trace_actor IS NULL AND NOT trace_curator)
        OR (trace_ref IS NOT NULL AND trace_ref ~ '^trace:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
            AND trace_actor IS NOT NULL AND trace_actor <> '' AND agent_run_id IS NOT NULL AND agent_run_id <> '')
    );
ALTER TABLE investigation_attempts ADD COLUMN trace_outcome text
    CHECK (trace_outcome IN ('used', 'unavailable'));

CREATE TABLE trace_records (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    id uuid NOT NULL,
    owner_actor text NOT NULL CHECK (owner_actor ~ '^[A-Za-z0-9][A-Za-z0-9_.@-]{0,127}$'),
    agent_run_id text NOT NULL CHECK (length(agent_run_id) BETWEEN 1 AND 200),
    schema_version integer NOT NULL CHECK (schema_version = 1),
    content_digest text NOT NULL CHECK (content_digest ~ '^[a-f0-9]{64}$'),
    sanitised_payload jsonb NOT NULL CHECK (jsonb_typeof(sanitised_payload) = 'object'),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL CHECK (expires_at >= created_at),
    status text NOT NULL DEFAULT 'ready' CHECK (status = 'ready'),
    PRIMARY KEY (workspace_id, id)
);
CREATE INDEX trace_records_expiry ON trace_records(workspace_id, expires_at);
ALTER TABLE trace_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE trace_records FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON trace_records
    USING (workspace_id = current_setting('akf.workspace_id', true))
    WITH CHECK (workspace_id = current_setting('akf.workspace_id', true));
