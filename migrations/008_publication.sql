ALTER TABLE workspaces ADD COLUMN publication_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE workspaces ADD COLUMN active_release_id uuid;
ALTER TABLE workspaces ADD COLUMN publication_hold boolean NOT NULL DEFAULT true;
ALTER TABLE gap_reports ADD COLUMN proof_kind text CHECK (proof_kind IN ('legacy-unverified', 'retrieval'));
UPDATE gap_reports SET proof_kind = 'legacy-unverified' WHERE state = 'verified';
ALTER TABLE proposals ADD CONSTRAINT proposals_workspace_id_unique UNIQUE (workspace_id, id);
ALTER TABLE gap_reports ADD CONSTRAINT gap_reports_workspace_id_unique UNIQUE (workspace_id, id);
ALTER TABLE proposals ADD COLUMN forge_repository text;
ALTER TABLE proposals ADD COLUMN forge_installation text;
ALTER TABLE proposals ADD COLUMN forge_base_branch text;
ALTER TABLE proposals ADD COLUMN forge_base_sha text;
CREATE UNIQUE INDEX proposal_forge_pr ON proposals (workspace_id, forge_repository, git_pr_number)
    WHERE forge_repository IS NOT NULL AND git_pr_number IS NOT NULL;

CREATE TABLE publication_operations (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    id uuid NOT NULL,
    trigger text NOT NULL CHECK (trigger IN ('push', 'pr-merge', 'pr-head', 'reconcile', 'bootstrap', 'operator')),
    repository text NOT NULL,
    branch text NOT NULL,
    installation text NOT NULL DEFAULT '',
    observed_sha text NOT NULL DEFAULT '',
    proposal_id uuid,
    delivery_id text,
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','validating','staged','rejected','promoted','superseded','held')),
    lease_token bigint NOT NULL DEFAULT 0,
    lease_until timestamptz,
    available_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, id),
    UNIQUE (workspace_id, delivery_id),
    FOREIGN KEY (workspace_id, proposal_id) REFERENCES proposals(workspace_id, id),
    FOREIGN KEY (workspace_id, delivery_id) REFERENCES webhook_deliveries(workspace_id, delivery_id)
);
CREATE INDEX publication_pending ON publication_operations(workspace_id, available_at)
    WHERE state IN ('queued', 'validating', 'staged');
CREATE UNIQUE INDEX publication_periodic_pending ON publication_operations(workspace_id)
    WHERE trigger = 'reconcile' AND state IN ('queued', 'validating', 'staged');

CREATE TABLE publication_candidates (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    id uuid NOT NULL,
    operation_id uuid,
    tree_digest text NOT NULL,
    expires_at timestamptz NOT NULL DEFAULT now() + interval '1 day',
    PRIMARY KEY (workspace_id,id),
    FOREIGN KEY (workspace_id,operation_id) REFERENCES publication_operations(workspace_id,id)
);

CREATE TABLE publication_receipts (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    candidate_id uuid NOT NULL,
    operation_id uuid,
    proposal_id uuid,
    lease_token bigint NOT NULL,
    baseline_sha text NOT NULL,
    generation bigint NOT NULL,
    git_sha text NOT NULL,
    tree_digest text NOT NULL,
    policy_digest text NOT NULL,
    dataset_digest text NOT NULL,
    evaluator_digest text NOT NULL,
    outcome text NOT NULL CHECK (outcome IN ('passed','rejected')),
    reason text NOT NULL,
    results jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, candidate_id),
    FOREIGN KEY (workspace_id, operation_id) REFERENCES publication_operations(workspace_id, id),
    FOREIGN KEY (workspace_id, proposal_id) REFERENCES proposals(workspace_id, id)
);

CREATE TABLE knowledge_releases (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    id uuid NOT NULL,
    repository text NOT NULL,
    git_sha text NOT NULL,
    tree_digest text NOT NULL,
    policy_digest text NOT NULL,
    dataset_digest text NOT NULL,
    receipt_id uuid NOT NULL,
    installed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, id),
    UNIQUE (workspace_id, repository, git_sha, tree_digest, policy_digest, dataset_digest),
    FOREIGN KEY (workspace_id, receipt_id) REFERENCES publication_receipts(workspace_id, candidate_id)
);
ALTER TABLE workspaces ADD FOREIGN KEY (id, active_release_id) REFERENCES knowledge_releases(workspace_id, id);

ALTER TABLE workspaces ADD COLUMN active_publication_receipt_id uuid;
ALTER TABLE workspaces ADD FOREIGN KEY (id, active_publication_receipt_id)
    REFERENCES publication_receipts(workspace_id, candidate_id);

CREATE TABLE proposal_merge_links (
    workspace_id text NOT NULL,
    proposal_id uuid NOT NULL,
    pr_number integer NOT NULL,
    merge_sha text NOT NULL,
    release_id uuid NOT NULL,
    head_sha text NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, proposal_id, pr_number, merge_sha),
    FOREIGN KEY (workspace_id, proposal_id) REFERENCES proposals(workspace_id, id),
    FOREIGN KEY (workspace_id, release_id) REFERENCES knowledge_releases(workspace_id, id)
);

CREATE TABLE gap_retrieval_cases (
    workspace_id text NOT NULL,
    gap_id uuid NOT NULL,
    case_id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, gap_id, case_id),
    FOREIGN KEY (workspace_id, gap_id) REFERENCES gap_reports(workspace_id, id)
);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['publication_operations','publication_candidates','publication_receipts','knowledge_releases','proposal_merge_links','gap_retrieval_cases']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format(
            'CREATE POLICY workspace_isolation ON %I USING (workspace_id = current_setting(''akf.workspace_id'', true)) WITH CHECK (workspace_id = current_setting(''akf.workspace_id'', true))', t);
    END LOOP;
END $$;
