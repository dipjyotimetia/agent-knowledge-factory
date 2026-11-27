package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/evaluations"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/knowledge/okf"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/knowledge/skills"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publication"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrCriticalRegression means a previously passing critical case fails on the proposal.
var ErrCriticalRegression = errors.New("critical evaluation regression")

func (s *Steps) vetProposal(ctx context.Context, prepared Prepared, files map[string]string) error {
	if s.KnowledgeRoot == "" {
		return nil
	}
	root, err := materialize(s.KnowledgeRoot, files)
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	bundle, err := okf.Load(root, okf.Options{})
	if err != nil {
		return err
	}
	if !bundle.Conformant() {
		return fmt.Errorf("proposed knowledge bundle is not conformant")
	}
	loaded, err := skills.LoadTree(root)
	if err != nil {
		return err
	}
	for _, skill := range loaded {
		if !skill.Valid() {
			return fmt.Errorf("proposed skill %s is not valid", skill.Name)
		}
	}
	if err := s.recordValidation(ctx, prepared.PropID, "okf-skills", "pass", map[string]any{"skills": len(loaded)}); err != nil {
		return err
	}
	if s.CasesPath == "" || s.Index == nil {
		return nil
	}
	cases, err := evaluations.Load(s.CasesPath)
	if err != nil {
		return err
	}
	if err := s.storeCases(ctx, cases); err != nil {
		return err
	}
	before, err := evaluations.RunRetrieval(ctx, cases, s.search(nil))
	if err != nil {
		return err
	}
	after, err := evaluations.RunRetrieval(ctx, cases, s.search(files))
	if err != nil {
		return err
	}
	if err := s.recordEval(ctx, prepared.PropID, "before", before); err != nil {
		return err
	}
	if err := s.recordEval(ctx, prepared.PropID, "after", after); err != nil {
		return err
	}
	if regressed(before, after) {
		_ = s.recordValidation(ctx, prepared.PropID, "critical-regression", "fail", map[string]any{"failed": after.Failed})
		return ErrCriticalRegression
	}
	return s.recordValidation(ctx, prepared.PropID, "critical-regression", "pass", map[string]any{"passed": after.Passed})
}

func regressed(before, after evaluations.Report) bool {
	passed := map[string]bool{}
	for _, result := range before.Results {
		if result.Critical && result.Passed {
			passed[result.ID] = true
		}
	}
	for _, result := range after.Results {
		if passed[result.ID] && !result.Passed {
			return true
		}
	}
	return false
}

func (s *Steps) search(overlay map[string]string) evaluations.SearchFunc {
	return s.searchHits(overlay, "")
}

func (s *Steps) searchSHA(sha string) evaluations.SearchFunc {
	return s.searchHits(nil, sha)
}

func (s *Steps) searchHits(overlay map[string]string, sha string) evaluations.SearchFunc {
	return func(ctx context.Context, query string) ([]evaluations.Hit, error) {
		var hits []index.Hit
		var err error
		if sha == "" {
			hits, err = s.Index.Search(ctx, s.Workspace, query, "", 10)
		} else {
			hits, err = s.Index.SearchAt(ctx, s.Workspace, sha, query, "", 10)
		}
		if err != nil {
			return nil, err
		}
		var out []evaluations.Hit
		for _, hit := range hits {
			revision := sha
			item, err := s.Index.Get(ctx, s.Workspace, hit.URI, revision)
			if err != nil {
				return nil, err
			}
			body := item.Body
			if overlay != nil {
				if repl, ok := overlay[item.Path]; ok {
					body = repl
				}
			}
			out = append(out, evaluations.Hit{URI: item.URI, Body: body})
		}
		return out, nil
	}
}

// Revalidate reruns the OKF, skill, and evaluation gate against the current pull-request head.
func (s *Steps) Revalidate(ctx context.Context, opened Opened) error {
	if s.Publication != nil {
		_, err := s.Publication.RequestRelease(ctx, publication.Request{Trigger: "pr-head", GapID: opened.GapID, PRNumber: opened.Number})
		return err
	}
	files, err := s.Publisher.BranchFiles(ctx, opened.Branch)
	if err != nil {
		return err
	}
	return s.vetProposal(ctx, opened.Prepared, files)
}

// Reconcile reindexes the default branch when it moved without a workflow signal.
// That covers a missed webhook and a force-push of the knowledge repository.
func (s *Steps) Reconcile(ctx context.Context) error {
	if s.Publication != nil {
		_, err := s.Publication.RequestRelease(ctx, publication.Request{Trigger: "reconcile"})
		return err
	}
	if s.Publisher == nil || s.Index == nil || s.DB == nil {
		return nil
	}
	head, err := s.Publisher.DefaultHead(ctx)
	if err != nil || head == "" {
		return err
	}
	var published string
	err = s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT published_sha FROM workspaces WHERE id = $1`, s.Workspace).Scan(&published)
	})
	if err != nil || published == head {
		return err
	}
	merged, err := s.Publisher.Tree(ctx, head)
	if err != nil {
		return err
	}
	root, err := materialize(s.KnowledgeRoot, merged)
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	if _, _, err := s.commitCandidate(ctx, head, root, ""); err != nil {
		return err
	}
	return s.verifyOpenGaps(ctx, head)
}

// commitCandidate indexes sha without publishing it, scores the candidate, and
// promotes it only when no critical case regresses. A regression leaves the
// previous published SHA in place.
func (s *Steps) commitCandidate(ctx context.Context, sha, root, proposalID string) (evaluations.Report, evaluations.Report, error) {
	var before, after evaluations.Report
	if s.Index == nil {
		return before, after, nil
	}
	if err := s.Index.Stage(ctx, s.Workspace, sha, root); err != nil {
		return before, after, err
	}
	if s.CasesPath != "" {
		cases, err := evaluations.Load(s.CasesPath)
		if err != nil {
			_ = s.Index.DropStage(ctx, s.Workspace, sha)
			return before, after, err
		}
		before, err = evaluations.RunRetrieval(ctx, cases, s.search(nil))
		if err != nil {
			_ = s.Index.DropStage(ctx, s.Workspace, sha)
			return before, after, err
		}
		after, err = evaluations.RunRetrieval(ctx, cases, s.searchSHA(sha))
		if err != nil {
			_ = s.Index.DropStage(ctx, s.Workspace, sha)
			return before, after, err
		}
		decisions, err := evaluations.RunDecisions(ctx, cases)
		if err != nil {
			_ = s.Index.DropStage(ctx, s.Workspace, sha)
			return before, after, err
		}
		before.Results = append(before.Results, decisions.Results...)
		after.Results = append(after.Results, decisions.Results...)
		if proposalID != "" {
			if err := s.recordEval(ctx, proposalID, "before", before); err != nil {
				_ = s.Index.DropStage(ctx, s.Workspace, sha)
				return before, after, err
			}
			if err := s.recordEval(ctx, proposalID, "after", after); err != nil {
				_ = s.Index.DropStage(ctx, s.Workspace, sha)
				return before, after, err
			}
		}
		if decisions.CriticalRegressions > 0 || regressed(before, after) {
			_ = s.Index.DropStage(ctx, s.Workspace, sha)
			return before, after, ErrCriticalRegression
		}
	}
	return before, after, s.Index.Promote(ctx, s.Workspace, sha)
}

func (s *Steps) verifyOpenGaps(ctx context.Context, sha string) error {
	if s.DB == nil || s.Gaps == nil {
		return nil
	}
	var ids []struct{ id, state string }
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT g.id::text, g.state
			FROM gap_reports g
			WHERE g.workspace_id = $1
			  AND g.state IN ('awaiting-review', 'accepted', 'published')
			  AND EXISTS (
			    SELECT 1 FROM proposals p WHERE g.id = ANY(p.gap_ids)
			  )
		`, s.Workspace)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item struct{ id, state string }
			if err := rows.Scan(&item.id, &item.state); err != nil {
				return err
			}
			ids = append(ids, item)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, gap := range ids {
		state := gap.state
		for _, next := range []string{domain.StateAccepted, domain.StatePublished, domain.StateVerified} {
			if !domain.CanTransition(state, next) {
				continue
			}
			updated, err := s.Gaps.Transition(ctx, s.Workspace, gap.id, next, "reconcile", sha)
			if err != nil {
				return err
			}
			state = updated.State
		}
	}
	return nil
}

func (s *Steps) recordValidation(ctx context.Context, proposalID, name, outcome string, details any) error {
	if s.DB == nil || proposalID == "" {
		return nil
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO validation_runs (id, workspace_id, proposal_id, check_name, outcome, details)
			VALUES ($1,$2,$3,$4,$5,$6)
		`, uuid.NewString(), s.Workspace, proposalID, name, outcome, raw)
		return err
	})
}

func (s *Steps) recordEval(ctx context.Context, proposalID, phase string, report evaluations.Report) error {
	if s.DB == nil || proposalID == "" {
		return nil
	}
	raw, err := json.Marshal(report.Results)
	if err != nil {
		return err
	}
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO evaluation_runs (
				id, workspace_id, proposal_id, phase, knowledge_sha, passed, failed, critical_regressions, results
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		`, uuid.NewString(), s.Workspace, proposalID, phase, "proposal", report.Passed, report.Failed, report.CriticalRegressions, raw)
		return err
	})
}

func (s *Steps) storeCases(ctx context.Context, cases []evaluations.Case) error {
	if s.DB == nil {
		return nil
	}
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		for _, c := range cases {
			gold, err := json.Marshal(map[string]string{"answer": c.Answer, "decision": c.Decision})
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO evaluation_cases (
					id, workspace_id, name, kind, task_prompt, retrieval_query, gold, critical, expects_change
				) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
				ON CONFLICT (workspace_id, id) DO UPDATE SET gold = EXCLUDED.gold, critical = EXCLUDED.critical
			`, c.ID, s.Workspace, c.ID, c.Kind, c.RetrievalQuery, c.RetrievalQuery, gold, c.Critical, c.ExpectsChange); err != nil {
				return err
			}
		}
		return nil
	})
}
