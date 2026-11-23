package postgres_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkspaceIsolation(t *testing.T) {
	ctx := context.Background()
	adminDSN := adminDatabaseURL()
	ensureAppRole(t, adminDSN)
	resetWorkspaces(t, adminDSN)

	admin, err := postgres.Open(ctx, adminDSN)
	if err != nil {
		t.Fatalf("installer database: %v", err)
	}
	admin.Installer = true
	t.Cleanup(admin.Close)
	if err := admin.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := admin.GrantAppRole(ctx, "akf_app"); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"banking-demo", "other-demo"} {
		if err := admin.CreateWorkspace(ctx, postgres.Workspace{
			ID:            id,
			DisplayName:   id,
			KnowledgeRepo: "knowledge-" + id,
		}); err != nil {
			t.Fatal(err)
		}
	}

	app, err := postgres.Open(ctx, appDatabaseURL())
	if err != nil {
		t.Fatalf("app database: %v", err)
	}
	t.Cleanup(app.Close)
	if err := app.CreateWorkspace(ctx, postgres.Workspace{ID: "nope-demo", DisplayName: "x", KnowledgeRepo: "x"}); err == nil {
		t.Fatal("API connection created a workspace")
	}

	got, err := app.GetWorkspace(ctx, "banking-demo")
	if err != nil {
		t.Fatal(err)
	}
	if got.KnowledgeRepo != "knowledge-banking-demo" {
		t.Fatalf("workspace = %+v", got)
	}

	err = app.WithWorkspace(ctx, "banking-demo", func(ctx context.Context, tx pgx.Tx) error {
		var id string
		scanErr := tx.QueryRow(ctx, "SELECT id FROM workspaces WHERE id = $1", "other-demo").Scan(&id)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return postgres.ErrNotFound
		}
		return scanErr
	})
	if !errors.Is(err, postgres.ErrNotFound) {
		t.Fatalf("cross-workspace read err = %v", err)
	}

	var ids []string
	err = app.WithWorkspace(ctx, "banking-demo", func(ctx context.Context, tx pgx.Tx) error {
		rows, queryErr := tx.Query(ctx, "SELECT id FROM workspaces")
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if scanErr := rows.Scan(&id); scanErr != nil {
				return scanErr
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "banking-demo" {
		t.Fatalf("RLS visible workspaces = %v, want only banking-demo", ids)
	}
}

func adminDatabaseURL() string {
	if dsn := os.Getenv("AKF_TEST_ADMIN_DSN"); dsn != "" {
		return dsn
	}
	return "postgres:///akf?host=/var/run/postgresql&sslmode=disable"
}

func appDatabaseURL() string {
	if dsn := os.Getenv("AKF_TEST_APP_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://akf_app:akf@127.0.0.1:5432/akf?sslmode=disable"
}

func ensureAppRole(t *testing.T, adminDSN string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `
		DO $$ BEGIN
			CREATE ROLE akf_app LOGIN PASSWORD 'akf' NOSUPERUSER NOBYPASSRLS;
		EXCEPTION WHEN duplicate_object THEN NULL;
		END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "GRANT CONNECT ON DATABASE akf TO akf_app"); err != nil {
		t.Fatal(err)
	}
}

func resetWorkspaces(t *testing.T, adminDSN string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	_, _ = pool.Exec(ctx, "DELETE FROM workspaces WHERE id IN ('banking-demo', 'other-demo', 'nope-demo')")
}
