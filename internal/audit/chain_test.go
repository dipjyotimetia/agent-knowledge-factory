package audit_test

import (
	"context"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/audit"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/testutil"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAuditHashChain(t *testing.T) {
	admin, app := testutil.OpenMigrated(t)
	ctx := context.Background()
	if err := admin.CreateWorkspace(ctx, postgres.Workspace{ID: "audit-demo", DisplayName: "Audit", KnowledgeRepo: "k"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool, err := pgxpool.New(context.Background(), testutil.AdminDSN())
		if err != nil {
			return
		}
		defer pool.Close()
		_, _ = pool.Exec(context.Background(), "DELETE FROM audit_events WHERE workspace_id = 'audit-demo'")
		_, _ = pool.Exec(context.Background(), "DELETE FROM workspaces WHERE id = 'audit-demo'")
	})
	err := app.WithWorkspace(ctx, "audit-demo", func(ctx context.Context, tx pgx.Tx) error {
		if _, err := audit.Append(ctx, tx, "audit-demo", audit.Event{
			Actor: "curator", Action: "propose", ObjectType: "gap", ObjectID: "g1", Details: map[string]any{"redacted": true},
		}); err != nil {
			return err
		}
		_, err := audit.Append(ctx, tx, "audit-demo", audit.Event{
			Actor: "reviewer", Action: "merge", ObjectType: "proposal", ObjectID: "p1", Details: map[string]any{"sha": "abc"},
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.WithWorkspace(ctx, "audit-demo", func(ctx context.Context, tx pgx.Tx) error {
		return audit.Verify(ctx, tx, "audit-demo")
	}); err != nil {
		t.Fatal(err)
	}
}
