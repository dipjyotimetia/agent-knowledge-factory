// Package runtime connects the control API to Temporal and the knowledge publisher.
package runtime

import (
	"context"
	"errors"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/api"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/client"
)

type deliveries struct {
	db        *postgres.DB
	workspace string
}

// NewDeliveries records each GitHub delivery id once for the workspace.
func NewDeliveries(db *postgres.DB, workspace string) api.DeliveryStore {
	return deliveries{db: db, workspace: workspace}
}

func (d deliveries) Remember(ctx context.Context, deliveryID string) (bool, error) {
	fresh := false
	err := d.db.WithWorkspace(ctx, d.workspace, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO webhook_deliveries (workspace_id, delivery_id)
			VALUES ($1, $2)
			ON CONFLICT DO NOTHING
		`, d.workspace, deliveryID)
		if err != nil {
			return err
		}
		fresh = tag.RowsAffected() == 1
		return nil
	})
	return fresh, err
}

type reviews struct {
	client    client.Client
	workspace string
	steps     *workflows.Steps
	reason    error
}

// NewReviews signals the curator workflow and reconciles a push to the default branch.
// A missing knowledge repository or database is returned by Reconcile instead of a silent success.
func NewReviews(c client.Client, cfg config.Config, db *postgres.DB, knowledge *index.Service, lookup func(string) string) api.ReviewSignaler {
	signaler := reviews{client: c, workspace: cfg.Workspace.ID}
	if db == nil || knowledge == nil {
		signaler.reason = errors.New("reconcile requires the database")
		return signaler
	}
	publisher, err := publish.GitHubFromConfig(cfg, lookup)
	if err != nil {
		signaler.reason = err
		return signaler
	}
	signaler.steps = &workflows.Steps{
		Workspace:     cfg.Workspace.ID,
		KnowledgeRoot: cfg.Workspace.KnowledgeRoot,
		DB:            db,
		Index:         knowledge,
		Publisher:     publisher,
	}
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
	if r.steps == nil {
		return errors.New("reconcile is not configured")
	}
	return r.steps.Reconcile(ctx)
}
