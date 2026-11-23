package workflows_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/testutil"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPublishRequiresVerifiedMerge(t *testing.T) {
	admin, app := testutil.OpenMigrated(t)
	ctx := context.Background()
	reset(t, "banking-demo")
	root := filepath.Join(repoRoot(t), "examples/banking/fixture-knowledge")
	if err := admin.CreateWorkspace(ctx, postgres.Workspace{
		ID: "banking-demo", DisplayName: "Banking", KnowledgeRepo: "knowledge-factory-demo-knowledge", KnowledgeRoot: root,
	}); err != nil {
		t.Fatal(err)
	}
	idx := index.New(app)
	if err := idx.Reindex(ctx, "banking-demo", "fixture-sha", root); err != nil {
		t.Fatal(err)
	}
	before, err := idx.Get(ctx, "banking-demo", "kb://banking/skills/movement-event-dedupe", "")
	if err != nil || !strings.Contains(before.Body, "`movementId`") {
		t.Fatalf("before err=%v body=%s", err, before.Body)
	}
	store := gaps.NewService(app)
	gap, err := store.Report(ctx, domain.GapReport{
		WorkspaceID: "banking-demo", AgentRunID: "run-1", TaskKind: "integration-testing",
		Category: domain.CategoryStaleInstruction, Summary: "The movement-event dedupe guide is incorrect",
		Observed:      "The authoritative event contract uses eventId for deduplication",
		Expected:      "The retrieved guide identifies a different key",
		KnowledgeRefs: []string{"kb://banking/events/movement-lifecycle"}, ClientEventID: "loop-1",
	}, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	steps := &workflows.Steps{
		Workspace: "banking-demo", KnowledgeRoot: root, Gaps: store, DB: app, Index: idx,
		Publisher: publish.NewMemory("fixture-sha", map[string]string{"placeholder": "x"}),
		Rules:     []curator.Rule{curator.BankingDedupeRule()},
		Evidence:  evidence(t),
	}
	repo := steps.Publisher.(*publish.Memory)
	prepared, err := steps.Prepare(ctx, gap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Decision != "update" {
		t.Fatalf("decision = %s conflicts=%v", prepared.Decision, prepared.Proposal.Conflicts)
	}
	var diff string
	var snaps int
	if err := app.WithWorkspace(ctx, "banking-demo", func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT diff FROM proposals WHERE id = $1`, prepared.PropID).Scan(&diff); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM source_snapshots WHERE workspace_id = 'banking-demo'`).Scan(&snaps)
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "eventId") || snaps < 1 {
		t.Fatalf("diff=%q snapshots=%d", diff, snaps)
	}
	opened, err := steps.Open(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}
	again, err := steps.Prepare(ctx, gap.ID)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := steps.Open(ctx, again)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Number != opened.Number {
		t.Fatalf("retry opened pull request %d, want %d", reopened.Number, opened.Number)
	}
	revised := strings.Replace(opened.Files["skills/movement-event-dedupe/SKILL.md"], "deduplicate by `movementId`", "deduplicate by `eventId`", 1)
	revised += "\nREVIEWED_SENTENCE\n"
	if err := repo.Revise(opened.Number, "skills/movement-event-dedupe/SKILL.md", revised); err != nil {
		t.Fatal(err)
	}
	opened.Files["skills/movement-event-dedupe/SKILL.md"] = "garbage from the activity input"
	still, err := idx.Get(ctx, "banking-demo", "kb://banking/skills/movement-event-dedupe", "")
	if err != nil || !strings.Contains(still.Body, "`movementId`") {
		t.Fatal("opening a pull request changed published knowledge")
	}
	if _, err := steps.Publish(ctx, opened, workflows.ReviewSignal{Merged: true, MergeSHA: "not-the-head"}); !errors.Is(err, publish.ErrNotMerged) {
		t.Fatalf("forged merge err = %v", err)
	}
	merged, err := repo.HumanMerge(opened.Number)
	if err != nil {
		t.Fatal(err)
	}
	published, err := steps.Publish(ctx, opened, workflows.ReviewSignal{Merged: true, MergeSHA: merged})
	if err != nil {
		t.Fatal(err)
	}
	if !published.Published || published.Revision != merged {
		t.Fatalf("published = %+v", published)
	}
	after, err := idx.Get(ctx, "banking-demo", "kb://banking/skills/movement-event-dedupe", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after.Body, "deduplicate by `eventId`") || strings.Contains(after.Body, "deduplicate by `movementId`") {
		t.Fatalf("after = %s", after.Body)
	}
	if !strings.Contains(after.Body, "REVIEWED_SENTENCE") {
		t.Fatal("publish indexed the proposal text instead of the merged file")
	}
}

func TestCreateSkillPersistsAProposalBeforeOpen(t *testing.T) {
	admin, app := testutil.OpenMigrated(t)
	ctx := context.Background()
	reset(t, "skill-demo")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.md"), []byte("---\nokf_version: \"0.3\"\n---\n\n# Empty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := admin.CreateWorkspace(ctx, postgres.Workspace{ID: "skill-demo", DisplayName: "Skill", KnowledgeRepo: "knowledge", KnowledgeRoot: root}); err != nil {
		t.Fatal(err)
	}
	store := gaps.NewService(app)
	gap, err := store.Report(ctx, domain.GapReport{
		WorkspaceID: "skill-demo", AgentRunID: "run-skill", TaskKind: "authoring",
		Category: domain.CategoryMissingProcedure, Summary: "No dedupe procedure is written down",
		Observed: "The contract says eventId", Expected: "A skill states the dedupe key",
		ClientEventID: "skill-1",
	}, "agent")
	if err != nil {
		t.Fatal(err)
	}
	ev := evidence(t)
	ev = append(ev, curator.Evidence{
		ID: "policy", Location: "github://acme/policy.md", Revision: strings.Repeat("d", 40), Digest: "sha256:policy",
		Classification: "policy", Content: "internal policy text",
	})
	steps := &workflows.Steps{
		Workspace: "skill-demo", KnowledgeRoot: root, Gaps: store, DB: app, Index: index.New(app),
		Publisher: publish.NewMemory("empty", nil),
		Rules:     []curator.Rule{curator.BankingDedupeRule()},
		Evidence:  ev,
	}
	prepared, err := steps.Prepare(ctx, gap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Decision != "create-skill" || prepared.PropID == "" || prepared.State == domain.StateInsufficientEvidence {
		t.Fatalf("prepared = %+v", prepared)
	}
	var class, verification string
	if err := app.WithWorkspace(ctx, "skill-demo", func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT classification, verification_state FROM evidence_links
			WHERE workspace_id = 'skill-demo' AND classification = 'policy'
		`).Scan(&class, &verification)
	}); err != nil {
		t.Fatal(err)
	}
	if class != "policy" || verification == "verified" {
		t.Fatalf("policy link class=%s verification=%s", class, verification)
	}
	opened, err := steps.Open(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Number == 0 {
		t.Fatal("pull request was not opened")
	}
	blank := prepared
	blank.PropID = ""
	if _, err := steps.Open(ctx, blank); err == nil {
		t.Fatal("opened a pull request without a proposal id")
	}
}

func evidence(t *testing.T) []curator.Evidence {
	t.Helper()
	root := repoRoot(t)
	return []curator.Evidence{
		{
			ID: "readme", Location: "github://dipjyotimetia/distributed-systems/contracts/avro/README.md",
			Revision: "8e6680d683ac728f0f089bd60e4a96576ab4ed45", Digest: "sha256:readme",
			Classification: "contract", Content: mustRead(t, filepath.Join(root, "internal/ingestion/github/testdata/readme.md")),
		},
		{
			ID: "schema", Location: "github://dipjyotimetia/distributed-systems/contracts/avro/movement-lifecycle-event.avsc",
			Revision: "8e6680d683ac728f0f089bd60e4a96576ab4ed45", Digest: "sha256:schema",
			Classification: "contract", Content: mustRead(t, filepath.Join(root, "internal/ingestion/github/testdata/movement-lifecycle-event.avsc")),
		},
	}
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
		"feedback", "evidence_links", "gap_events", "validation_runs", "evaluation_runs",
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
