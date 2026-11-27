package demo_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/testutil"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// TestLiveTemporalLoop runs the banking investigation on a real Temporal server
// and the in-memory forge. It is skipped unless TEMPORAL_ADDRESS is set.
func TestLiveTemporalLoop(t *testing.T) {
	address := os.Getenv("TEMPORAL_ADDRESS")
	if address == "" {
		t.Skip("TEMPORAL_ADDRESS is unset")
	}
	admin, app := testutil.OpenMigrated(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	reset(t, "temporal-demo")
	root := filepath.Join(repoRoot(t), "examples/banking/fixture-knowledge")
	if err := admin.CreateWorkspace(ctx, postgres.Workspace{
		ID: "temporal-demo", DisplayName: "Temporal", KnowledgeRepo: "knowledge", KnowledgeRoot: root,
	}); err != nil {
		t.Fatal(err)
	}
	idx := index.NewFixture(app)
	if err := idx.Reindex(ctx, "temporal-demo", "fixture-sha", root); err != nil {
		t.Fatal(err)
	}
	store := gaps.NewService(app)
	gap, err := store.Report(ctx, domain.GapReport{
		WorkspaceID: "temporal-demo", AgentRunID: "run-live", TaskKind: "integration-testing",
		Category: domain.CategoryStaleInstruction, Summary: "The movement-event dedupe guide is incorrect",
		Observed: "The authoritative event contract uses eventId for deduplication", Expected: "The retrieved guide identifies a different key",
		KnowledgeRefs: []string{"kb://banking/events/movement-lifecycle"}, ClientEventID: "live-1",
	}, "qe")
	if err != nil {
		t.Fatal(err)
	}
	repo := publish.NewMemory("fixture-sha", map[string]string{"placeholder": "x"})
	steps := &workflows.Steps{
		Workspace: "temporal-demo", KnowledgeRoot: root, Gaps: store, DB: app, Index: idx,
		Publisher: repo, Rules: []curator.Rule{curator.BankingDedupeRule()}, Evidence: recordedEvidence(t),
		CasesPath: filepath.Join(repoRoot(t), "examples/banking/tests/cases.json"),
	}
	var temporalClient client.Client
	dialDeadline := time.Now().Add(90 * time.Second)
	for {
		temporalClient, err = client.Dial(client.Options{HostPort: address})
		if err == nil {
			break
		}
		if time.Now().After(dialDeadline) {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
	}
	defer temporalClient.Close()
	w := worker.New(temporalClient, "knowledge-curator-test", worker.Options{})
	w.RegisterWorkflow(workflows.TriageGapWorkflow)
	w.RegisterActivity(steps)
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	run, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: "curate-temporal-demo-" + gap.ID, TaskQueue: "knowledge-curator-test",
	}, workflows.TriageGapWorkflow, workflows.Input{GapID: gap.ID})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	var merged string
	for time.Now().Before(deadline) {
		current, err := store.Status(ctx, "temporal-demo", gap.ID)
		if err == nil && current.State == domain.StateAwaitingReview {
			merged, err = repo.HumanMerge(1)
			if err != nil {
				t.Fatal(err)
			}
			if err := temporalClient.SignalWorkflow(ctx, run.GetID(), run.GetRunID(), workflows.SignalReview, workflows.ReviewSignal{Merged: true, MergeSHA: merged}); err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if merged == "" {
		t.Fatal("workflow did not reach review")
	}
	var result workflows.Result
	if err := run.Get(ctx, &result); err != nil {
		t.Fatal(err)
	}
	replay23(t, ctx, temporalClient, run.GetID(), workflows.TriageGapWorkflow, "")
	if !result.Published || result.State != domain.StateVerified {
		t.Fatalf("result = %+v", result)
	}
}
