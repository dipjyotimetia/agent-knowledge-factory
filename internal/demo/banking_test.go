package demo_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/evaluations"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/testutil"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBankingLearningCycle(t *testing.T) {
	admin, app := testutil.OpenMigrated(t)
	ctx := context.Background()
	reset(t, "banking-demo")
	root := filepath.Join(repoRoot(t), "examples/banking/fixture-knowledge")
	cases, err := evaluations.Load(filepath.Join(repoRoot(t), "examples/banking/tests/cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.CreateWorkspace(ctx, postgres.Workspace{
		ID: "banking-demo", DisplayName: "Banking demo", KnowledgeRepo: "knowledge-factory-demo-knowledge", KnowledgeRoot: root,
	}); err != nil {
		t.Fatal(err)
	}
	idx := index.New(app)
	if err := idx.Reindex(ctx, "banking-demo", "fixture-sha", root); err != nil {
		t.Fatal(err)
	}
	before, err := evaluations.RunRetrieval(ctx, cases, search(idx))
	if err != nil {
		t.Fatal(err)
	}
	if find(t, before, "mov-dedupe-key").Passed {
		t.Fatal("baseline already retrieved eventId")
	}
	passedBefore := map[string]bool{}
	for _, result := range before.Results {
		if result.Critical && result.Passed {
			passedBefore[result.ID] = true
		}
	}

	store := gaps.NewService(app)
	report := domain.GapReport{
		WorkspaceID: "banking-demo", AgentRunID: "run-qe-1", TaskKind: "integration-testing",
		Category: domain.CategoryStaleInstruction, Summary: "The movement-event dedupe guide is incorrect",
		Observed:      "The authoritative event contract uses eventId for deduplication",
		Expected:      "The retrieved guide identifies a different key",
		KnowledgeRefs: []string{"kb://banking/events/movement-lifecycle"},
		EvidenceRefs:  []string{"github://dipjyotimetia/distributed-systems/contracts/avro/README.md"},
		ClientEventID: "qe-1", Severity: "medium",
	}
	first, err := store.Report(ctx, report, "qe-agent")
	if err != nil {
		t.Fatal(err)
	}
	report.ClientEventID = "qe-2"
	report.AgentRunID = "run-qe-2"
	second, err := store.Report(ctx, report, "other-agent")
	if err != nil {
		t.Fatal(err)
	}
	canonical, linked, err := store.Linked(ctx, "banking-demo", second.ID)
	if err != nil || canonical.ID != first.ID || len(linked) != 1 {
		t.Fatalf("investigation canonical=%s linked=%d err=%v", canonical.ID, len(linked), err)
	}

	repo := publish.NewMemory("fixture-sha", map[string]string{"placeholder": "x"})
	steps := &workflows.Steps{
		Workspace: "banking-demo", KnowledgeRoot: root, Gaps: store, DB: app, Index: idx,
		Publisher: repo, Rules: []curator.Rule{curator.BankingDedupeRule()}, Evidence: recordedEvidence(t),
	}
	prepared, err := steps.Prepare(ctx, first.ID)
	if err != nil || prepared.Decision != "update" {
		t.Fatalf("prepare %+v err=%v", prepared.Decision, err)
	}
	opened, err := steps.Open(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}
	mid, err := idx.Get(ctx, "banking-demo", "kb://banking/skills/movement-event-dedupe", "")
	if err != nil || !strings.Contains(mid.Body, "deduplicate by `movementId`") {
		t.Fatal("pull request was visible to default search")
	}
	merged, err := repo.HumanMerge(opened.Number)
	if err != nil {
		t.Fatal(err)
	}
	published, err := steps.Publish(ctx, opened, workflows.ReviewSignal{Merged: true, MergeSHA: merged})
	if err != nil || !published.Published {
		t.Fatalf("publish %+v err=%v", published, err)
	}
	after, err := evaluations.RunRetrieval(ctx, cases, search(idx))
	if err != nil {
		t.Fatal(err)
	}
	if !find(t, after, "mov-dedupe-key").Passed {
		t.Fatalf("dedupe did not improve: %+v", find(t, after, "mov-dedupe-key"))
	}
	for _, result := range after.Results {
		if passedBefore[result.ID] && !result.Passed {
			t.Fatalf("critical regression on %s", result.ID)
		}
	}
	verified, err := store.Transition(ctx, "banking-demo", first.ID, domain.StateVerified, "evaluator", "critical dedupe case improved and no critical regression")
	if err != nil || verified.State != domain.StateVerified {
		t.Fatalf("verified %+v err=%v", verified, err)
	}
	events, err := store.Events(ctx, "banking-demo", first.ID)
	if err != nil || len(events) < 4 {
		t.Fatalf("audit chain too short: %+v %v", events, err)
	}
	item, err := idx.Get(ctx, "banking-demo", "kb://banking/events/movement-lifecycle", "")
	if err != nil || !strings.Contains(item.Body, "deduplicate by `eventId`") || item.Revision != merged {
		t.Fatalf("published concept = %s rev %s", item.Body, item.Revision)
	}
	if err := idx.Reindex(ctx, "banking-demo", "rolled-back", root); err != nil {
		t.Fatal(err)
	}
	rolled, err := idx.Get(ctx, "banking-demo", "kb://banking/skills/movement-event-dedupe", "")
	if err != nil || !strings.Contains(rolled.Body, "deduplicate by `movementId`") {
		t.Fatal("rollback did not restore the previous knowledge tree")
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
			out = append(out, evaluations.Hit{URI: item.URI, Body: item.Body})
		}
		return out, nil
	}
}

func recordedEvidence(t *testing.T) []curator.Evidence {
	t.Helper()
	root := repoRoot(t)
	return []curator.Evidence{
		{ID: "readme", Location: "github://dipjyotimetia/distributed-systems/contracts/avro/README.md", Revision: "8e6680d683ac728f0f089bd60e4a96576ab4ed45", Digest: "sha256:readme", Classification: "contract", Content: mustRead(t, filepath.Join(root, "internal/ingestion/github/testdata/readme.md"))},
		{ID: "schema", Location: "github://dipjyotimetia/distributed-systems/contracts/avro/movement-lifecycle-event.avsc", Revision: "8e6680d683ac728f0f089bd60e4a96576ab4ed45", Digest: "sha256:schema", Classification: "contract", Content: mustRead(t, filepath.Join(root, "internal/ingestion/github/testdata/movement-lifecycle-event.avsc"))},
	}
}

func find(t *testing.T, report evaluations.Report, id string) evaluations.Result {
	t.Helper()
	for _, result := range report.Results {
		if result.ID == id {
			return result
		}
	}
	t.Fatalf("missing %s", id)
	return evaluations.Result{}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
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
		"query_logs", "knowledge_search_index", "knowledge_items", "webhook_deliveries", "audit_events",
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
