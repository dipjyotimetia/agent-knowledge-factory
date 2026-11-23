package main

import (
	"context"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/client"
)

type deliveryDB interface {
	WithWorkspace(ctx context.Context, workspaceID string, fn func(context.Context, pgx.Tx) error) error
}

type pgDeliveries struct {
	db        deliveryDB
	workspace string
}

func (p pgDeliveries) Remember(ctx context.Context, deliveryID string) (bool, error) {
	fresh := false
	err := p.db.WithWorkspace(ctx, p.workspace, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO webhook_deliveries (workspace_id, delivery_id)
			VALUES ($1, $2)
			ON CONFLICT DO NOTHING
		`, p.workspace, deliveryID)
		if err != nil {
			return err
		}
		fresh = tag.RowsAffected() == 1
		return nil
	})
	return fresh, err
}

type temporalReviews struct {
	client    client.Client
	workspace string
	steps     *workflows.Steps
}

func (t temporalReviews) Signal(ctx context.Context, gapID string, signal workflows.ReviewSignal) error {
	return t.client.SignalWorkflow(ctx, workflows.WorkflowID(t.workspace, gapID), "", workflows.SignalReview, signal)
}

func (t temporalReviews) Reconcile(ctx context.Context) error {
	if t.steps == nil {
		return nil
	}
	return t.steps.Reconcile(ctx)
}
