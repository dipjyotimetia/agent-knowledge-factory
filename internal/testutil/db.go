// Package testutil opens the local Postgres used by integration tests.
package testutil

import (
	"context"
	"os"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AdminDSN is the superuser connection used only to migrate and create workspaces.
func AdminDSN() string {
	if dsn := os.Getenv("AKF_TEST_ADMIN_DSN"); dsn != "" {
		return dsn
	}
	return "postgres:///akf?host=/var/run/postgresql&sslmode=disable"
}

// AppDSN is the non-superuser API role.
func AppDSN() string {
	if dsn := os.Getenv("AKF_TEST_APP_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://akf_app:akf@127.0.0.1:5432/akf?sslmode=disable"
}

// OpenMigrated migrates as the installer and returns the API connection.
func OpenMigrated(t *testing.T) (admin *postgres.DB, app *postgres.DB) {
	t.Helper()
	ctx := context.Background()
	ensureRole(t)
	admin, err := postgres.Open(ctx, AdminDSN())
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
	app, err = postgres.Open(ctx, AppDSN())
	if err != nil {
		t.Fatalf("app database: %v", err)
	}
	t.Cleanup(app.Close)
	return admin, app
}

func ensureRole(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, AdminDSN())
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
