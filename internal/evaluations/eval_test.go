package evaluations_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/evaluations"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDatasetAndBeforeAfter(t *testing.T) {
	cases, err := evaluations.Load(filepath.Join(repoRoot(t), "examples/banking/tests/cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 20 {
		t.Fatalf("cases = %d", len(cases))
	}
	kinds := map[string]int{}
	for _, c := range cases {
		kinds[c.Kind]++
		prompt := evaluations.CuratorPrompt(c, nil)
		if strings.Contains(prompt, c.Sentinel) || strings.Contains(prompt, c.Answer) && c.Answer != "" && c.Extract == "dedupe-key" {
			if strings.Contains(prompt, c.Sentinel) {
				t.Fatalf("prompt leaked sentinel for %s", c.ID)
			}
		}
		if strings.Contains(prompt, c.Sentinel) {
			t.Fatalf("sentinel leaked for %s", c.ID)
		}
	}
	if kinds["documentation-gap"] != 10 || kinds["contradiction"] != 5 || kinds["negative"] != 5 {
		t.Fatalf("kinds = %v", kinds)
	}
	decisions, err := evaluations.RunDecisions(context.Background(), cases)
	if err != nil {
		t.Fatal(err)
	}
	if decisions.Failed != 0 {
		t.Fatalf("decision failures %+v", decisions.Results)
	}

	admin, app := testutil.OpenMigrated(t)
	ctx := context.Background()
	reset(t, "banking-demo")
	root := filepath.Join(repoRoot(t), "examples/banking/fixture-knowledge")
	if err := admin.CreateWorkspace(ctx, postgres.Workspace{ID: "banking-demo", DisplayName: "Banking", KnowledgeRepo: "knowledge", KnowledgeRoot: root}); err != nil {
		t.Fatal(err)
	}
	svc := index.New(app)
	if err := svc.Reindex(ctx, "banking-demo", "before", root); err != nil {
		t.Fatal(err)
	}
	before, err := evaluations.RunRetrieval(ctx, cases, search(svc))
	if err != nil {
		t.Fatal(err)
	}
	dedupe := findResult(t, before, "mov-dedupe-key")
	if dedupe.Passed {
		t.Fatal("stale index already answered eventId")
	}
	if before.CriticalRegressions == 0 {
		t.Fatal("expected the critical dedupe miss to count")
	}
}

func search(svc *index.Service) evaluations.SearchFunc {
	return func(ctx context.Context, query string) ([]evaluations.Hit, error) {
		hits, err := svc.Search(ctx, "banking-demo", query, "", 3)
		if err != nil {
			return nil, err
		}
		var out []evaluations.Hit
		for _, hit := range hits {
			item, err := svc.Get(ctx, "banking-demo", hit.URI, "")
			if err != nil {
				return nil, err
			}
			out = append(out, evaluations.Hit{URI: hit.URI, Body: item.Body})
		}
		return out, nil
	}
}

func findResult(t *testing.T, report evaluations.Report, id string) evaluations.Result {
	t.Helper()
	for _, result := range report.Results {
		if result.ID == id {
			return result
		}
	}
	t.Fatalf("missing result %s", id)
	return evaluations.Result{}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod")
		}
		dir = parent
	}
}

func reset(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testutil.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, table := range []string{
		"knowledge_search_index", "knowledge_items", "webhook_deliveries", "audit_events",
		"feedback", "evidence_links", "gap_events", "validation_runs", "evaluation_runs",
		"evaluation_cases", "proposals", "gap_reports", "agent_runs", "source_snapshots",
		"source_connections", "workspaces",
	} {
		query := "DELETE FROM " + table + " WHERE workspace_id = $1"
		if table == "workspaces" {
			query = "DELETE FROM workspaces WHERE id = $1"
		}
		if _, err := pool.Exec(ctx, query, id); err != nil {
			t.Fatal(err)
		}
	}
}
