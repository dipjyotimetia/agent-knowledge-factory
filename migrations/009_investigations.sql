CREATE TABLE investigation_attempts (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    id uuid NOT NULL,
    gap_id uuid NOT NULL,
    base_release_id uuid,
    policy_version text NOT NULL,
    state text NOT NULL CHECK (state IN ('queued','running','review','parked','completed','failed','cancelled')),
    started_at timestamptz,
    review_at timestamptz,
    reminded_at timestamptz,
    evidence_ready boolean NOT NULL DEFAULT false,
    feedback_ready boolean NOT NULL DEFAULT false,
    legacy_review boolean NOT NULL DEFAULT false,
    proposal_id uuid,
    evaluation_receipt_id uuid,
    target_case_ids text[] NOT NULL DEFAULT '{}',
    result text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id,id),
    FOREIGN KEY (workspace_id,gap_id) REFERENCES gap_reports(workspace_id,id),
    FOREIGN KEY (workspace_id,base_release_id) REFERENCES knowledge_releases(workspace_id,id),
    FOREIGN KEY (workspace_id,proposal_id) REFERENCES proposals(workspace_id,id),
    FOREIGN KEY (workspace_id,evaluation_receipt_id) REFERENCES publication_receipts(workspace_id,candidate_id)
);
CREATE UNIQUE INDEX investigation_one_live ON investigation_attempts(workspace_id,gap_id)
    WHERE state IN ('queued','running','review','parked');
CREATE INDEX investigation_queue ON investigation_attempts(workspace_id,state,created_at,id);

CREATE TABLE operation_ledger (
    workspace_id text NOT NULL,
    attempt_id uuid NOT NULL,
    kind text NOT NULL CHECK (kind IN ('prepare','open','model','pr')),
    operation_key text NOT NULL,
    request_digest text NOT NULL,
    status text NOT NULL CHECK (status IN ('pending','completed','failed')),
    attempt_count int NOT NULL DEFAULT 0 CHECK (attempt_count>=0),
    lease_token bigint NOT NULL DEFAULT 0,
    lease_until timestamptz,
    external_ref text NOT NULL DEFAULT '',
    error_code text NOT NULL DEFAULT '',
    PRIMARY KEY (workspace_id,attempt_id,kind,operation_key),
    FOREIGN KEY (workspace_id,attempt_id) REFERENCES investigation_attempts(workspace_id,id)
);

ALTER TABLE source_snapshots ADD CONSTRAINT source_snapshots_workspace_id_unique UNIQUE(workspace_id,id);
CREATE TABLE attempt_evidence (
    workspace_id text NOT NULL,
    attempt_id uuid NOT NULL,
    evidence_id text NOT NULL,
    snapshot_id uuid NOT NULL,
    digest text NOT NULL,
    PRIMARY KEY (workspace_id,attempt_id,evidence_id),
    FOREIGN KEY (workspace_id,attempt_id) REFERENCES investigation_attempts(workspace_id,id),
    FOREIGN KEY (workspace_id,snapshot_id) REFERENCES source_snapshots(workspace_id,id)
);

ALTER TABLE feedback ADD CONSTRAINT feedback_workspace_id_unique UNIQUE(workspace_id,id);
CREATE TABLE attempt_feedback (
    workspace_id text NOT NULL,
    attempt_id uuid NOT NULL,
    feedback_id uuid NOT NULL,
    PRIMARY KEY (workspace_id,attempt_id,feedback_id),
    UNIQUE (workspace_id,feedback_id),
    FOREIGN KEY (workspace_id,attempt_id) REFERENCES investigation_attempts(workspace_id,id),
    FOREIGN KEY (workspace_id,feedback_id) REFERENCES feedback(workspace_id,id)
);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['investigation_attempts','operation_ledger','attempt_evidence','attempt_feedback']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format(
            'CREATE POLICY workspace_isolation ON %I USING (workspace_id = current_setting(''akf.workspace_id'', true)) WITH CHECK (workspace_id = current_setting(''akf.workspace_id'', true))',t);
    END LOOP;
END $$;
