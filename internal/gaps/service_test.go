package gaps_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDedupeIdempotencySecretAndTransition(t *testing.T) {
	admin, app := testutil.OpenMigrated(t)
	ctx := context.Background()
	resetGaps(t, "banking-demo", "other-demo")
	if err := admin.CreateWorkspace(ctx, postgres.Workspace{ID: "banking-demo", DisplayName: "Banking", KnowledgeRepo: "knowledge"}); err != nil {
		t.Fatal(err)
	}
	if err := admin.CreateWorkspace(ctx, postgres.Workspace{ID: "other-demo", DisplayName: "Other", KnowledgeRepo: "other"}); err != nil {
		t.Fatal(err)
	}
	svc := gaps.NewService(app)
	first := sampleReport("agent-a-event")
	a, err := svc.Report(ctx, first, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if a.State != domain.StateNew {
		t.Fatalf("state = %s", a.State)
	}
	again, err := svc.Report(ctx, first, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != a.ID {
		t.Fatalf("idempotent id = %s want %s", again.ID, a.ID)
	}
	second := sampleReport("agent-b-event")
	b, err := svc.Report(ctx, second, "agent-b")
	if err != nil {
		t.Fatal(err)
	}
	if b.State != domain.StateDuplicate || b.CanonicalID != a.ID {
		t.Fatalf("duplicate = %+v", b)
	}
	canonical, related, err := svc.Linked(ctx, "banking-demo", b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if canonical.ID != a.ID || len(related) != 1 || related[0].ID != b.ID {
		t.Fatalf("canonical=%s related=%+v", canonical.ID, related)
	}
	if _, err := svc.Status(ctx, "other-demo", a.ID); err == nil {
		t.Fatal("other workspace read the banking gap")
	}
	triaged, err := svc.Transition(ctx, "banking-demo", a.ID, domain.StateTriaged, "curator", "queued")
	if err != nil || triaged.State != domain.StateTriaged {
		t.Fatalf("triage = %+v err=%v", triaged, err)
	}
	if _, err := svc.Transition(ctx, "banking-demo", a.ID, domain.StatePublished, "curator", "skip"); err == nil {
		t.Fatal("illegal transition succeeded")
	}
	events, err := svc.Events(ctx, "banking-demo", a.ID)
	if err != nil || len(events) < 2 {
		t.Fatalf("events = %+v err=%v", events, err)
	}

	secret := sampleReport("secret-event")
	secret.Observed = "leaked AKIAIOSFODNN7EXAMPLE in the trace"
	if _, err := svc.Report(ctx, secret, "agent-a"); !errors.Is(err, gaps.ErrSecret) {
		t.Fatalf("secret err = %v", err)
	}
	found, err := svc.AuditContains(ctx, "banking-demo", "AKIAIOSFODNN7EXAMPLE")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("audit stored the secret")
	}
}

func TestConcurrentDuplicate(t *testing.T) {
	admin, app := testutil.OpenMigrated(t)
	ctx := context.Background()
	resetGaps(t, "banking-demo")
	if err := admin.CreateWorkspace(ctx, postgres.Workspace{ID: "banking-demo", DisplayName: "Banking", KnowledgeRepo: "knowledge"}); err != nil {
		t.Fatal(err)
	}
	svc := gaps.NewService(app)
	var wg sync.WaitGroup
	ids := make([]string, 2)
	errs := make([]error, 2)
	for i, agent := range []string{"agent-a", "agent-b"} {
		wg.Add(1)
		go func(i int, agent string) {
			defer wg.Done()
			report := sampleReport(agent + "-race")
			gap, err := svc.Report(ctx, report, agent)
			ids[i] = gap.ID
			errs[i] = err
		}(i, agent)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	states := map[string]int{}
	for _, id := range ids {
		gap, err := svc.Status(ctx, "banking-demo", id)
		if err != nil {
			t.Fatal(err)
		}
		states[gap.State]++
	}
	if states[domain.StateNew] != 1 || states[domain.StateDuplicate] != 1 {
		t.Fatalf("states = %v", states)
	}
}

func sampleReport(eventID string) domain.GapReport {
	return domain.GapReport{
		WorkspaceID:   "banking-demo",
		AgentRunID:    "run-" + eventID,
		TaskKind:      "integration-testing",
		Category:      domain.CategoryStaleInstruction,
		Summary:       "The movement-event dedupe guide is incorrect",
		Observed:      "The authoritative event contract uses eventId for deduplication",
		Expected:      "The retrieved guide identifies a different key",
		KnowledgeRefs: []string{"kb://banking/events/movement-lifecycle"},
		ClientEventID: eventID,
		Severity:      "medium",
	}
}

func resetGaps(t *testing.T, ids ...string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testutil.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, id := range ids {
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
}
