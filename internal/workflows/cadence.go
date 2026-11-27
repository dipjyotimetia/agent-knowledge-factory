package workflows

import (
	"context"
	"time"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/workflow"
)

// DrainOpenWorkflow refetches evidence and proposes for gaps that are still new or triaged.
// A second start for the same gap is ignored by Prepare and Open.
func DrainOpenWorkflow(ctx workflow.Context) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
	})
	var steps *Steps
	if err := workflow.ExecuteActivity(ctx, steps.RefreshEvidence).Get(ctx, nil); err != nil {
		return err
	}
	if err := workflow.ExecuteActivity(ctx, steps.RaiseContradictions).Get(ctx, nil); err != nil {
		return err
	}
	var ids []string
	if err := workflow.ExecuteActivity(ctx, steps.ListOpen).Get(ctx, &ids); err != nil {
		return err
	}
	for _, id := range ids {
		var prepared Prepared
		if err := workflow.ExecuteActivity(ctx, steps.Prepare, id).Get(ctx, &prepared); err != nil {
			return err
		}
		if prepared.PropID == "" {
			continue
		}
		if err := workflow.ExecuteActivity(ctx, steps.Open, prepared).Get(ctx, nil); err != nil {
			return err
		}
	}
	return nil
}

// EvidenceLoader fetches pinned sources. The worker sets it so each cadence run refetches.
type EvidenceLoader func(context.Context) ([]curator.Evidence, error)

func (s *Steps) RefreshEvidence(ctx context.Context) error {
	if s.FetchEvidence == nil {
		return nil
	}
	_, err := s.FetchEvidence(ctx)
	return err
}

// ListOpen returns gap ids that have not yet reached a proposal.
func (s *Steps) ListOpen(ctx context.Context) ([]string, error) {
	if s.DB == nil {
		return nil, nil
	}
	var ids []string
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id::text FROM gap_reports
			WHERE workspace_id = $1 AND state IN ('new', 'triaged')
			ORDER BY created_at
		`, s.Workspace)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

// RaiseContradictions files one gap per corpus conflict. Repeating the scan is idempotent.
func (s *Steps) RaiseContradictions(ctx context.Context) error {
	if s.Gaps == nil || s.KnowledgeRoot == "" {
		return nil
	}
	files, err := readTree(s.KnowledgeRoot)
	if err != nil {
		return err
	}
	conflicts, err := curator.ScanClaims(files, curator.DefaultClaimPatterns())
	if err != nil {
		return err
	}
	for _, conflict := range conflicts {
		_, err := s.Gaps.Report(ctx, domain.GapReport{
			WorkspaceID:   s.Workspace,
			AgentRunID:    "corpus-scan",
			TaskKind:      "contradiction-scan",
			Category:      domain.CategoryContradiction,
			Summary:       "Corpus conflict: " + conflict.Name,
			Observed:      conflict.Summary(),
			Expected:      "One value for " + conflict.Name,
			ClientEventID: "contradiction-" + conflict.Name,
		}, "curator")
		if err != nil {
			return err
		}
	}
	return nil
}
