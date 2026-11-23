package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/evaluations"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/knowledge/okf"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/knowledge/skills"
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
	return func(ctx context.Context, query string) ([]evaluations.Hit, error) {
		hits, err := s.Index.Search(ctx, s.Workspace, query, "", 3)
		if err != nil {
			return nil, err
		}
		var out []evaluations.Hit
		for _, hit := range hits {
			item, err := s.Index.Get(ctx, s.Workspace, hit.URI, "")
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

func (s *Steps) loadEval(ctx context.Context, proposalID, phase string) (evaluations.Report, error) {
	var report evaluations.Report
	if s.DB == nil || proposalID == "" {
		return report, nil
	}
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		var raw []byte
		err := tx.QueryRow(ctx, `
			SELECT results FROM evaluation_runs
			WHERE proposal_id = $1 AND phase = $2
			ORDER BY created_at DESC LIMIT 1
		`, proposalID, phase).Scan(&raw)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &report.Results)
	})
	return report, err
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
