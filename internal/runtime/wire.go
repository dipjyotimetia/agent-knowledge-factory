// Package runtime connects the control API to Temporal and the knowledge publisher.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/api"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publication"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

type reviews struct {
	client      client.Client
	workspace   string
	publication *publication.Service
	reason      error
}

// NewReviews signals the curator workflow and reconciles a push to the default branch.
// A missing knowledge repository or database is returned by Reconcile instead of a silent success.
func NewReviews(c client.Client, cfg config.Config, db *postgres.DB, knowledge *index.Service, lookup func(string) string) api.ReviewSignaler {
	signaler := reviews{client: c, workspace: cfg.Workspace.ID}
	if db == nil || knowledge == nil {
		signaler.reason = errors.New("reconcile requires the database")
		return signaler
	}
	service, err := NewPublication(cfg, db, knowledge, lookup)
	if err != nil {
		signaler.reason = err
		return signaler
	}
	signaler.publication = service
	return signaler
}

func (r reviews) Signal(ctx context.Context, gapID string, signal workflows.ReviewSignal) error {
	if r.client == nil {
		return errors.New("temporal client is not configured")
	}
	return r.client.SignalWorkflow(ctx, workflows.WorkflowID(r.workspace, gapID), "", workflows.SignalReview, signal)
}

func (r reviews) Reconcile(ctx context.Context) error {
	if r.reason != nil {
		return r.reason
	}
	if r.publication == nil {
		return errors.New("reconcile is not configured")
	}
	_, err := r.publication.RequestRelease(ctx, publication.Request{Trigger: "reconcile"})
	return err
}

// NewPublication wires intake and the worker to the same release configuration.
func NewPublication(cfg config.Config, db *postgres.DB, knowledge *index.Service, lookup func(string) string) (*publication.Service, error) {
	forge, err := publish.GitHubFromConfig(cfg, lookup)
	if err != nil {
		return nil, err
	}
	branch := cfg.Workspace.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	policy, err := json.Marshal(struct {
		Sources []config.Source
		Rules   []string
	}{cfg.Sources, cfg.Curation.Rules})
	if err != nil {
		return nil, err
	}
	return &publication.Service{
		DB: db, Index: knowledge, Forge: forge, Workspace: cfg.Workspace.ID,
		Identity:  publish.Identity{Repository: strings.ToLower(forge.Owner + "/" + forge.Repo), Installation: forge.Installation, Branch: branch},
		CasesPath: cfg.Curation.CasesPath, PolicyDigest: publication.Digest(policy),
		Sources: func(ctx context.Context) (publication.EvidenceSet, error) {
			values, err := workflows.EvidenceFromConfig(ctx, cfg, lookup)
			return publication.EvidenceSet(values), err
		},
	}, nil
}

// SignalLegacyClose completes outstanding V1 review waits during the KF-22 upgrade.
func (r reviews) SignalLegacyClose(ctx context.Context, number int, branch string) (bool, error) {
	if r.publication == nil || branch == "" {
		return false, nil
	}
	var gap string
	err := r.publication.DB.WithWorkspace(ctx, r.workspace, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT gap_ids[1]::text FROM proposals
          WHERE workspace_id=$1 AND git_pr_number=$2 AND git_branch=$3 AND forge_repository IS NULL
          AND cardinality(gap_ids)=1 AND git_branch='curator/'||gap_ids[1]::text`, r.workspace, number, branch).Scan(&gap)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = r.Signal(ctx, gap, workflows.ReviewSignal{Rejected: true, Reason: "pull request closed"})
	var missing *serviceerror.NotFound
	if errors.As(err, &missing) {
		return true, nil
	}
	return true, err
}
