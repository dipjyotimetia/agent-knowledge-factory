package index_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/audit"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/evaluations"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/testutil"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSearchIsPublishedWorkspaceScoped(t *testing.T) {
	admin, app := testutil.OpenMigrated(t)
	ctx := context.Background()
	root := filepath.Join(repoRoot(t), "examples/banking/fixture-knowledge")
	reset(t, "banking-demo", "other-demo")
	mustWorkspace(t, admin, postgres.Workspace{ID: "banking-demo", DisplayName: "Banking", KnowledgeRepo: "knowledge-factory-demo-knowledge", KnowledgeRoot: root})
	mustWorkspace(t, admin, postgres.Workspace{ID: "other-demo", DisplayName: "Other", KnowledgeRepo: "other", KnowledgeRoot: otherRoot(t)})

	svc := index.New(app)
	if err := svc.Reindex(ctx, "banking-demo", "fixture-sha", root); err != nil {
		t.Fatal(err)
	}
	applied, err := svc.SyncDelivery(ctx, "banking-demo", "delivery-1", "fixture-sha", root)
	if err != nil || !applied {
		t.Fatalf("sync applied=%v err=%v", applied, err)
	}
	applied, err = svc.SyncDelivery(ctx, "banking-demo", "delivery-1", "fixture-sha", root)
	if err != nil || applied {
		t.Fatalf("duplicate delivery applied=%v err=%v", applied, err)
	}
	if err := svc.Reindex(ctx, "other-demo", "other-sha", otherRoot(t)); err != nil {
		t.Fatal(err)
	}

	hits, err := svc.Search(ctx, "banking-demo", "dedupe movement", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("expected the stale dedupe guide")
	}
	foundLifecycle := false
	for _, hit := range hits {
		if strings.Contains(hit.URI, "draft-note") || strings.Contains(hit.URI, "legacy-json") {
			t.Fatalf("unapproved or superseded hit %s", hit.URI)
		}
		if strings.Contains(hit.URI, "UNIQUE_OTHER") || hit.Revision != "fixture-sha" {
			t.Fatalf("hit leaked or has wrong revision: %+v", hit)
		}
		if strings.HasSuffix(hit.URI, "/events/movement-lifecycle") {
			foundLifecycle = true
			if hit.LastReviewed == "" {
				t.Fatal("search hit omitted last_reviewed")
			}
			if !hit.Stale {
				t.Fatal("stale guide was returned without a stale warning")
			}
			if len(hit.Sources) == 0 {
				t.Fatal("provenance missing")
			}
		}
	}
	if !foundLifecycle {
		t.Fatalf("hits = %+v", hits)
	}

	draft, err := svc.Search(ctx, "banking-demo", "DRAFT_ONLY_MARKER", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(draft) != 0 {
		t.Fatalf("draft returned: %+v", draft)
	}
	other, err := svc.Search(ctx, "banking-demo", "UNIQUE_OTHER_WORKSPACE", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("cross-workspace hit: %+v", other)
	}

	item, err := svc.Get(ctx, "banking-demo", "kb://banking/events/movement-lifecycle", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(item.Body, "movementId") || item.Revision != "fixture-sha" {
		t.Fatalf("item = %+v", item)
	}
	if _, err := svc.Get(ctx, "banking-demo", "kb://banking/events/movement-lifecycle", "not-published"); err == nil {
		t.Fatal("unpublished revision was readable")
	}

	// A second SHA without the lifecycle document becomes the published view.
	trimmed := t.TempDir()
	copyFile(t, filepath.Join(root, "index.md"), filepath.Join(trimmed, "index.md"))
	if err := svc.Reindex(ctx, "banking-demo", "trimmed-sha", trimmed); err != nil {
		t.Fatal(err)
	}
	gone, err := svc.Search(ctx, "banking-demo", "deduplicate movementId", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 0 {
		t.Fatalf("deleted document still searchable: %+v", gone)
	}
	// The previous SHA is retained but is not the default read.
	if _, err := svc.Get(ctx, "banking-demo", "kb://banking/events/movement-lifecycle", ""); err == nil {
		t.Fatal("default get returned a document that is not in the published SHA")
	}
	var marked string
	if err := app.WithWorkspace(ctx, "banking-demo", func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT superseded_by FROM knowledge_items
			WHERE git_sha = 'fixture-sha' AND uri = 'kb://banking/events/movement-lifecycle'
		`).Scan(&marked)
	}); err != nil {
		t.Fatal(err)
	}
	if marked != "trimmed-sha" {
		t.Fatalf("previous revision superseded_by = %q", marked)
	}
	secretHits, err := svc.Search(ctx, "banking-demo", "AKIAIOSFODNN7EXAMPLE", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(secretHits) != 0 {
		t.Fatal("a secret-bearing query was executed")
	}
	var stored string
	if err := app.WithWorkspace(ctx, "banking-demo", func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT query_redacted FROM query_logs ORDER BY created_at DESC LIMIT 1`).Scan(&stored)
	}); err != nil {
		t.Fatal(err)
	}
	if stored != "[redacted]" || strings.Contains(stored, "AKIA") {
		t.Fatalf("query log = %q", stored)
	}
}

func otherRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := "---\nokf_version: \"0.3\"\nx-akf-uri-prefix: kb://other\n---\n\n# Other\n"
	if err := os.WriteFile(filepath.Join(dir, "index.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	note := "---\ntype: Note\ntitle: Other\nstatus: stable\n---\n\nUNIQUE_OTHER_WORKSPACE token.\n"
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestFixtureAnswersSurviveRanking(t *testing.T) {
	admin, app := testutil.OpenMigrated(t)
	ctx := context.Background()
	root := filepath.Join(repoRoot(t), "examples/banking/fixture-knowledge")
	reset(t, "rank-demo")
	mustWorkspace(t, admin, postgres.Workspace{ID: "rank-demo", DisplayName: "Rank", KnowledgeRepo: "knowledge", KnowledgeRoot: root})
	svc := index.New(app)
	if err := svc.Reindex(ctx, "rank-demo", "fixture-sha", root); err != nil {
		t.Fatal(err)
	}
	cases, err := evaluations.Load(filepath.Join(repoRoot(t), "examples/banking/tests/cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := evaluations.RunRetrieval(ctx, cases, func(ctx context.Context, query string) ([]evaluations.Hit, error) {
		hits, err := svc.Search(ctx, "rank-demo", query, "", 10)
		if err != nil {
			return nil, err
		}
		var out []evaluations.Hit
		for _, hit := range hits {
			item, err := svc.Get(ctx, "rank-demo", hit.URI, "")
			if err != nil {
				return nil, err
			}
			out = append(out, evaluations.Hit{URI: item.URI, Body: item.Body})
		}
		return out, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"topic-name": false, "status-values": false, "occurred-at": false}
	for _, result := range report.Results {
		if _, ok := want[result.ID]; ok {
			want[result.ID] = result.Passed
			if !result.Passed {
				t.Errorf("%s failed: %s", result.ID, result.Detail)
			}
		}
	}
}

func TestRollbackRepublishesThePreviousSHA(t *testing.T) {
	admin, app := testutil.OpenMigrated(t)
	ctx := context.Background()
	root := filepath.Join(repoRoot(t), "examples/banking/fixture-knowledge")
	reset(t, "rollback-demo")
	mustWorkspace(t, admin, postgres.Workspace{ID: "rollback-demo", DisplayName: "Rollback", KnowledgeRepo: "knowledge", KnowledgeRoot: root})
	svc := index.New(app)
	if err := svc.Reindex(ctx, "rollback-demo", "sha-old", root); err != nil {
		t.Fatal(err)
	}
	if err := svc.Reindex(ctx, "rollback-demo", "sha-new", root); err != nil {
		t.Fatal(err)
	}
	hits, err := svc.Search(ctx, "rollback-demo", "dedupe movement", "", 5)
	if err != nil || len(hits) == 0 || hits[0].Revision != "sha-new" {
		t.Fatalf("published = %+v %v", hits, err)
	}
	if err := svc.Rollback(ctx, "rollback-demo", "sha-old", "test"); err != nil {
		t.Fatal(err)
	}
	hits, err = svc.Search(ctx, "rollback-demo", "dedupe movement", "", 5)
	if err != nil || len(hits) == 0 || hits[0].Revision != "sha-old" {
		t.Fatalf("rolled back = %+v %v", hits, err)
	}
	if err := svc.DB.WithWorkspace(ctx, "rollback-demo", func(ctx context.Context, tx pgx.Tx) error {
		return audit.Verify(ctx, tx, "rollback-demo")
	}); err != nil {
		t.Fatal(err)
	}
}

func mustWorkspace(t *testing.T, admin *postgres.DB, ws postgres.Workspace) {
	t.Helper()
	if err := admin.CreateWorkspace(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
}

func reset(t *testing.T, ids ...string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testutil.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tables := []string{
		"query_logs", "knowledge_search_index", "knowledge_items", "webhook_deliveries", "audit_events",
		"feedback", "evaluation_runs", "validation_runs", "evidence_links", "gap_events",
		"evaluation_cases", "proposals", "gap_reports", "agent_runs", "source_snapshots",
		"source_connections",
	}
	for _, id := range ids {
		for _, table := range tables {
			if _, err := pool.Exec(ctx, "DELETE FROM "+table+" WHERE workspace_id = $1", id); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pool.Exec(ctx, "DELETE FROM workspaces WHERE id = $1", id); err != nil {
			t.Fatal(err)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	body, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, body, 0o644); err != nil {
		t.Fatal(err)
	}
}
