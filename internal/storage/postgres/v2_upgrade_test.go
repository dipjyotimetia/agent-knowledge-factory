package postgres_test

import (
	"context"
	"io/fs"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestV2UpgradePreservesLegacyVerificationUnderAppRole(t *testing.T) {
	ctx := context.Background()
	admin, name := databaseAtMigration(t, "008", 7)
	const ws = "baseline-upgrade"
	if err := admin.CreateWorkspace(ctx, postgres.Workspace{ID: ws, DisplayName: "Upgrade", KnowledgeRepo: "fixture/knowledge", PublishedSHA: "legacy"}); err != nil {
		t.Fatal(err)
	}
	if err := admin.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO gap_reports(id,workspace_id,agent_id,task_kind,category,summary,observed,expected,
         client_event_id,state,dedupe_signature,failure_signature) VALUES ($1,$2,'fixture','test','stale-instruction','s','o','e','p','verified','d','f')`,
			uuid.NewString(), ws)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := admin.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := admin.GrantAppRole(ctx, "akf_app"); err != nil {
		t.Fatal(err)
	}
	app, err := postgres.Open(ctx, withDatabaseName(t, appDatabaseURL(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	if err := app.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		var held bool
		var sha, proof string
		var release *string
		if err := tx.QueryRow(ctx, `SELECT published_sha, publication_hold, active_release_id::text FROM workspaces WHERE id=$1`, ws).
			Scan(&sha, &held, &release); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT proof_kind FROM gap_reports WHERE workspace_id=$1`, ws).Scan(&proof); err != nil {
			return err
		}
		if sha != "legacy" || !held || release != nil || proof != "legacy-unverified" {
			t.Errorf("upgrade published=%q held=%t release=%v proof=%q", sha, held, release, proof)
		}
		var attemptCount int
		return tx.QueryRow(ctx, `SELECT count(*) FROM investigation_attempts WHERE workspace_id=$1`, ws).Scan(&attemptCount)
	}); err != nil {
		t.Fatal(err)
	}
}

func databaseAtMigration(t *testing.T, through string, expected int) (*postgres.DB, string) {
	t.Helper()
	ctx := context.Background()
	adminDSN := adminDatabaseURL()
	ensureAppRole(t, adminDSN)
	parent, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(parent.Close)
	name := "akf_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := parent.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := parent.Exec(ctx, "DROP DATABASE "+identifier); err != nil {
			t.Errorf("remove temporary upgrade database: %v", err)
		}
	})
	admin, err := postgres.Open(ctx, withDatabaseName(t, adminDSN, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	admin.Installer = true
	baseline := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() >= through {
			continue
		}
		data, err := fs.ReadFile(migrations.Files, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		baseline[entry.Name()] = &fstest.MapFile{Data: data}
	}
	if len(baseline) != expected {
		t.Fatalf("baseline contains %d migrations", len(baseline))
	}
	if err := admin.Migrate(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	return admin, name
}

func Test24UpgradeClearsOpaqueLegacyReferences(t *testing.T) {
	ctx := context.Background()
	admin, name := databaseAtMigration(t, "010", 9)
	for _, ws := range []string{"legacy-one", "legacy-two"} {
		if err := admin.CreateWorkspace(ctx, postgres.Workspace{ID: ws, DisplayName: ws}); err != nil {
			t.Fatal(err)
		}
		if err := admin.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
			// A path and an ID-shaped legacy value are both removed on upgrade.
			if _, err := tx.Exec(ctx, `INSERT INTO agent_runs(workspace_id,id,task_kind,trace_ref)
				VALUES ($1,'legacy','test','/private/nonexistent/legacy.json')`, ws); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO gap_reports(id,workspace_id,agent_id,agent_run_id,task_kind,category,summary,observed,expected,
				trace_ref,client_event_id,state,dedupe_signature,failure_signature)
				VALUES ($1,$2,'fixture','legacy','test','stale-instruction','s','o','e',$3,'p','new','d','f')`,
				uuid.NewString(), ws, "trace:"+uuid.NewString())
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := admin.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := admin.GrantAppRole(ctx, "akf_app"); err != nil {
		t.Fatal(err)
	}
	app, err := postgres.Open(ctx, withDatabaseName(t, appDatabaseURL(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	for _, ws := range []string{"legacy-one", "legacy-two"} {
		if err := app.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
			var gaps, runs, liveTraces int
			err := tx.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM gap_reports WHERE trace_ref IS NULL),
				(SELECT count(*) FROM agent_runs WHERE trace_ref IS NULL),
				(SELECT count(*) FROM trace_records)`).Scan(&gaps, &runs, &liveTraces)
			if err == nil && (gaps != 1 || runs != 1 || liveTraces != 0) {
				t.Fatalf("legacy migration counts: gaps=%d runs=%d traces=%d", gaps, runs, liveTraces)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func withDatabaseName(t *testing.T, dsn, name string) string {
	t.Helper()
	uri, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	uri.Path = "/" + name
	return uri.String()
}
