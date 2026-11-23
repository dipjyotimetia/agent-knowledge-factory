package workflows

import (
	"context"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/policy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// persistEvidence writes the pinned sources and the agent run that produced a proposal.
// Snapshot bodies stay in Postgres. Nothing here writes the knowledge Git repository.
func (s *Steps) persistEvidence(ctx context.Context, gap domain.Gap, prop curator.Proposal) error {
	if s.DB == nil {
		return nil
	}
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		if gap.Report.AgentRunID != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO agent_runs (workspace_id, id, task_kind, trace_ref, started_at, metadata)
				VALUES ($1, $2, $3, $4, now(), '{}')
				ON CONFLICT (workspace_id, id) DO NOTHING
			`, s.Workspace, gap.Report.AgentRunID, gap.Report.TaskKind, gap.Report.TraceRef); err != nil {
				return err
			}
		}
		connID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(s.Workspace+"/github")).String()
		if _, err := tx.Exec(ctx, `
			INSERT INTO source_connections (id, workspace_id, provider, scope, credential_ref)
			VALUES ($1, $2, 'github', '{"read":"allowlisted"}', 'env:GITHUB_READONLY_TOKEN')
			ON CONFLICT (id) DO NOTHING
		`, connID, s.Workspace); err != nil {
			return err
		}
		for _, ev := range s.Evidence {
			if ev.Classification != "contract" && ev.Classification != "policy" {
				continue
			}
			content := policy.Redact(ev.Content)
			class := ev.Classification
			if class == "" {
				class = "secondary"
			}
			verification := "unverified"
			if class == "contract" {
				verification = "verified"
			}
			snapID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(s.Workspace+"/"+ev.Location+"/"+ev.Revision)).String()
			if _, err := tx.Exec(ctx, `
				INSERT INTO source_snapshots (
					id, workspace_id, connection_id, location, digest, revision, observed_at, classification, content
				) VALUES ($1,$2,$3,$4,$5,$6, now(), $7, $8)
				ON CONFLICT (workspace_id, location, revision) DO UPDATE
				SET digest = EXCLUDED.digest, content = EXCLUDED.content, classification = EXCLUDED.classification, observed_at = now()
			`, snapID, s.Workspace, connID, ev.Location, ev.Digest, ev.Revision, class, content); err != nil {
				return err
			}
			linkID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(gap.ID+"/"+snapID)).String()
			if _, err := tx.Exec(ctx, `
				INSERT INTO evidence_links (
					id, workspace_id, gap_id, snapshot_id, location, revision, digest, classification, verification_state
				) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
				ON CONFLICT (id) DO NOTHING
			`, linkID, s.Workspace, gap.ID, snapID, ev.Location, ev.Revision, ev.Digest, class, verification); err != nil {
				return err
			}
		}
		return nil
	})
}
