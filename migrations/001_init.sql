-- Operational schema for the Agent Knowledge Factory.
-- Git remains the source of truth for approved knowledge. These tables are
-- metadata, event history, and a rebuildable search index.
-- Every tenant table is forced under row-level security keyed by the
-- transaction-local setting akf.workspace_id. The application sets that
-- value from the authenticated principal, never from an untrusted client id
-- alone. There is intentionally no bypass flag: a role that can set custom
-- GUCs must still be told the workspace by the trusted HTTP or CLI layer,
-- and installer writes run on a separate superuser connection that never
-- serves API traffic.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE workspaces (
    id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    knowledge_repo TEXT NOT NULL,
    knowledge_root TEXT NOT NULL DEFAULT '',
    published_sha TEXT NOT NULL DEFAULT '',
    security_policy JSONB NOT NULL DEFAULT '{}'::jsonb,
    curation_settings JSONB NOT NULL DEFAULT '{}'::jsonb,
    retention_days INTEGER NOT NULL DEFAULT 30 CHECK (retention_days >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT workspaces_id_format CHECK (
        char_length(id) BETWEEN 1 AND 64
        AND id ~ '^[a-z0-9]+(-[a-z0-9]+)*$'
    )
);

CREATE TABLE source_connections (
    id UUID PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    provider TEXT NOT NULL,
    scope JSONB NOT NULL,
    credential_ref TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT source_connections_credential_ref_not_secret CHECK (credential_ref !~ '-----BEGIN')
);

CREATE TABLE source_snapshots (
    id UUID PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    connection_id UUID REFERENCES source_connections (id),
    location TEXT NOT NULL,
    digest TEXT NOT NULL,
    revision TEXT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    acl JSONB NOT NULL DEFAULT '{}'::jsonb,
    content TEXT NOT NULL,
    classification TEXT NOT NULL DEFAULT 'secondary',
    expires_at TIMESTAMPTZ,
    UNIQUE (workspace_id, location, revision)
);

CREATE TABLE knowledge_items (
    id UUID PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    uri TEXT NOT NULL,
    item_type TEXT NOT NULL CHECK (item_type IN ('concept', 'skill')),
    concept_type TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    body TEXT NOT NULL,
    git_sha TEXT NOT NULL,
    path TEXT NOT NULL,
    status TEXT NOT NULL,
    trust_tier TEXT NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    approved BOOLEAN NOT NULL DEFAULT FALSE,
    superseded_by TEXT,
    stale BOOLEAN NOT NULL DEFAULT FALSE,
    warnings JSONB NOT NULL DEFAULT '[]'::jsonb,
    headings TEXT NOT NULL DEFAULT '',
    search_vector tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', coalesce(title, '')), 'A')
        || setweight(to_tsvector('english', coalesce(description, '')), 'B')
        || setweight(to_tsvector('english', coalesce(headings, '')), 'B')
        || setweight(to_tsvector('english', coalesce(body, '')), 'D')
    ) STORED,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, uri, git_sha)
);

CREATE INDEX knowledge_items_fts_idx ON knowledge_items USING GIN (search_vector);
CREATE INDEX knowledge_items_title_trgm_idx ON knowledge_items USING GIN (title gin_trgm_ops);
CREATE INDEX knowledge_items_workspace_sha_idx ON knowledge_items (workspace_id, git_sha);

-- Derived and rebuildable. Search reads this table, filtered to the workspace
-- published SHA, so a rebuild can swap the index without treating Git history
-- as lost.
CREATE TABLE knowledge_search_index (
    knowledge_item_id UUID PRIMARY KEY REFERENCES knowledge_items (id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    document tsvector NOT NULL,
    approved BOOLEAN NOT NULL,
    git_sha TEXT NOT NULL,
    status TEXT NOT NULL,
    superseded_by TEXT
);

CREATE INDEX knowledge_search_index_fts_idx ON knowledge_search_index USING GIN (document);
CREATE INDEX knowledge_search_index_scope_idx ON knowledge_search_index (workspace_id, git_sha, approved);

CREATE TABLE agent_runs (
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    id TEXT NOT NULL,
    task_kind TEXT NOT NULL,
    trace_ref TEXT,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (workspace_id, id)
);

CREATE TABLE gap_reports (
    id UUID PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    agent_id TEXT NOT NULL,
    agent_run_id TEXT,
    task_kind TEXT NOT NULL,
    category TEXT NOT NULL,
    summary TEXT NOT NULL,
    observed TEXT NOT NULL,
    expected TEXT NOT NULL,
    knowledge_refs TEXT[] NOT NULL DEFAULT '{}',
    evidence_refs TEXT[] NOT NULL DEFAULT '{}',
    trace_ref TEXT,
    client_event_id TEXT NOT NULL,
    reported_severity TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL,
    dedupe_signature TEXT NOT NULL,
    failure_signature TEXT NOT NULL,
    canonical_gap_id UUID REFERENCES gap_reports (id),
    source_revision TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, agent_id, client_event_id),
    CONSTRAINT gap_reports_state_chk CHECK (state IN (
        'new', 'triaged', 'investigating', 'proposal-ready', 'awaiting-review',
        'accepted', 'rejected', 'published', 'verified',
        'duplicate', 'insufficient-evidence', 'cancelled', 'failed'
    ))
);

-- One open investigation per failure signature inside a workspace.
-- Duplicate submissions point at the canonical row and are excluded here.
CREATE UNIQUE INDEX gap_reports_one_open_dedupe
    ON gap_reports (workspace_id, dedupe_signature)
    WHERE canonical_gap_id IS NULL
      AND state IN (
          'new', 'triaged', 'investigating', 'proposal-ready',
          'awaiting-review', 'accepted', 'published'
      );

CREATE INDEX gap_reports_workspace_state_idx ON gap_reports (workspace_id, state);

CREATE TABLE gap_events (
    id UUID PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    gap_id UUID NOT NULL REFERENCES gap_reports (id),
    from_state TEXT,
    to_state TEXT NOT NULL,
    actor TEXT NOT NULL,
    reason TEXT NOT NULL,
    correlation_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX gap_events_gap_idx ON gap_events (workspace_id, gap_id, created_at);

CREATE TABLE evidence_links (
    id UUID PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    gap_id UUID NOT NULL REFERENCES gap_reports (id),
    snapshot_id UUID REFERENCES source_snapshots (id),
    location TEXT NOT NULL,
    revision TEXT NOT NULL,
    digest TEXT NOT NULL DEFAULT '',
    classification TEXT NOT NULL,
    verification_state TEXT NOT NULL,
    excerpt TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT evidence_links_verification_chk CHECK (verification_state IN (
        'unverified', 'verified', 'rejected', 'forged'
    ))
);

CREATE TABLE proposals (
    id UUID PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    gap_ids UUID[] NOT NULL,
    source_revisions JSONB NOT NULL DEFAULT '[]'::jsonb,
    target_paths TEXT[] NOT NULL DEFAULT '{}',
    diff TEXT NOT NULL DEFAULT '',
    edits JSONB NOT NULL DEFAULT '[]'::jsonb,
    claim_evidence_map JSONB NOT NULL DEFAULT '[]'::jsonb,
    conflicts JSONB NOT NULL DEFAULT '[]'::jsonb,
    risk_level TEXT NOT NULL,
    validation_results JSONB NOT NULL DEFAULT '[]'::jsonb,
    evaluation_results JSONB NOT NULL DEFAULT '{}'::jsonb,
    git_pr_url TEXT NOT NULL DEFAULT '',
    git_branch TEXT NOT NULL DEFAULT '',
    head_sha TEXT NOT NULL DEFAULT '',
    approval_status TEXT NOT NULL,
    decision TEXT NOT NULL,
    model_name TEXT NOT NULL DEFAULT '',
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT proposals_approval_chk CHECK (approval_status IN (
        'draft', 'open', 'changes-requested', 'approved', 'rejected', 'merged'
    )),
    CONSTRAINT proposals_decision_chk CHECK (decision IN (
        'update', 'no-change', 'escalate', 'insufficient-evidence'
    ))
);

CREATE TABLE validation_runs (
    id UUID PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    proposal_id UUID NOT NULL REFERENCES proposals (id),
    check_name TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('pass', 'fail')),
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE evaluation_cases (
    id TEXT NOT NULL,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    name TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('documentation-gap', 'contradiction', 'negative')),
    task_prompt TEXT NOT NULL,
    retrieval_query TEXT NOT NULL DEFAULT '',
    gold JSONB NOT NULL,
    critical BOOLEAN NOT NULL DEFAULT FALSE,
    expects_change BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (workspace_id, id)
);

CREATE TABLE evaluation_runs (
    id UUID PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    proposal_id UUID REFERENCES proposals (id),
    phase TEXT NOT NULL CHECK (phase IN ('before', 'after')),
    knowledge_sha TEXT NOT NULL,
    passed INTEGER NOT NULL,
    failed INTEGER NOT NULL,
    critical_regressions INTEGER NOT NULL,
    results JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE audit_events (
    id UUID PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    actor TEXT NOT NULL,
    request_id TEXT NOT NULL,
    action TEXT NOT NULL,
    object_type TEXT NOT NULL,
    object_id TEXT NOT NULL,
    object_revision TEXT NOT NULL DEFAULT '',
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_events_workspace_idx ON audit_events (workspace_id, created_at);

CREATE TABLE feedback (
    id UUID PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    knowledge_uri TEXT NOT NULL,
    revision TEXT NOT NULL,
    observation TEXT NOT NULL,
    agent_run_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE webhook_deliveries (
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    delivery_id TEXT NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, delivery_id)
);

ALTER TABLE workspaces ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspaces FORCE ROW LEVEL SECURITY;
CREATE POLICY workspaces_isolation ON workspaces
    USING (id = current_setting('akf.workspace_id', true))
    WITH CHECK (id = current_setting('akf.workspace_id', true));

DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'source_connections',
        'source_snapshots',
        'knowledge_items',
        'knowledge_search_index',
        'agent_runs',
        'gap_reports',
        'gap_events',
        'evidence_links',
        'proposals',
        'validation_runs',
        'evaluation_cases',
        'evaluation_runs',
        'audit_events',
        'feedback',
        'webhook_deliveries'
    ]
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format(
            'CREATE POLICY %I ON %I USING (workspace_id = current_setting(''akf.workspace_id'', true)) WITH CHECK (workspace_id = current_setting(''akf.workspace_id'', true))',
            t || '_workspace',
            t
        );
    END LOOP;
END $$;
