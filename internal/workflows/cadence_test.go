package workflows_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/testutil"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
	"go.temporal.io/sdk/testsuite"
)

func TestDrainRefetchesEvidenceAndIsIdempotent(t *testing.T) {
	admin, app := testutil.OpenMigrated(t)
	ctx := context.Background()
	reset(t, "cadence-demo")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.md"), []byte("---\nokf_version: \"0.3\"\n---\n\n# Empty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := admin.CreateWorkspace(ctx, postgres.Workspace{ID: "cadence-demo", DisplayName: "Cadence", KnowledgeRepo: "knowledge", KnowledgeRoot: root}); err != nil {
		t.Fatal(err)
	}
	store := gaps.NewService(app)
	gap, err := store.Report(ctx, domain.GapReport{
		WorkspaceID: "cadence-demo", AgentRunID: "run-c", TaskKind: "authoring",
		Category: domain.CategoryMissingProcedure, Summary: "Need a dedupe skill",
		Observed: "contract says eventId", Expected: "a skill says eventId",
		ClientEventID: "cadence-1",
	}, "agent")
	if err != nil {
		t.Fatal(err)
	}
	fetches := 0
	steps := &workflows.Steps{
		Workspace: "cadence-demo", KnowledgeRoot: root, Gaps: store, DB: app, Index: index.New(app),
		Publisher: publish.NewMemory("empty", nil),
		Rules:     []curator.Rule{curator.BankingDedupeRule()},
		FetchEvidence: func(context.Context) ([]curator.Evidence, error) {
			fetches++
			return evidence(t), nil
		},
	}
	run := func() {
		t.Helper()
		var suite testsuite.WorkflowTestSuite
		env := suite.NewTestWorkflowEnvironment()
		env.RegisterActivity(steps)
		env.ExecuteWorkflow(workflows.DrainOpenWorkflow)
		if err := env.GetWorkflowError(); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if fetches != 1 {
		t.Fatalf("fetches = %d", fetches)
	}
	current, err := store.Status(ctx, "cadence-demo", gap.ID)
	if err != nil || current.State != domain.StateAwaitingReview {
		t.Fatalf("state = %+v %v", current.State, err)
	}
	run()
	if fetches != 2 {
		t.Fatalf("second fetches = %d", fetches)
	}
	repo := steps.Publisher.(*publish.Memory)
	if _, err := repo.BranchFiles(ctx, "curator/"+gap.ID); err != nil {
		t.Fatal(err)
	}
}
