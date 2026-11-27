package demo_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/auth"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/evaluations"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/investigation"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publication"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/testutil"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/traces"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/proto"
)

// A forge that accepts the first PR and drops the response.
type lostResponseForge struct {
	*publish.Memory
	once        sync.Once
	unavailable atomic.Bool
}

func (f *lostResponseForge) CreatePullRequest(ctx context.Context, r publish.Request) (publish.Result, error) {
	pr, err := f.Memory.CreatePullRequest(ctx, r)
	if err != nil {
		return pr, err
	}
	lost := false
	f.once.Do(func() { lost = true })
	if lost {
		return publish.Result{}, errors.New("simulated lost forge response")
	}
	return pr, nil
}

func (f *lostResponseForge) PullRequest(ctx context.Context, number int) (publish.PullRequest, error) {
	if f.unavailable.Load() {
		return publish.PullRequest{}, &publish.HTTPError{Method: "GET", Status: 403}
	}
	return f.Memory.PullRequest(ctx, number)
}

func Test23RestartDuringBankingReviewAndReplay(t *testing.T) {
	address := os.Getenv("TEMPORAL_ADDRESS")
	if address == "" {
		t.Skip("TEMPORAL_ADDRESS is unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, app := testutil.OpenMigrated(t)
	ws := "attempt-" + uuid.NewString()
	root := repoRoot(t)
	source := filepath.Join(root, "examples/banking/fixture-knowledge")
	files := map[string]string{}
	err := filepath.WalkDir(source, func(file string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		name, err := filepath.Rel(source, file)
		if err == nil {
			files[name] = mustRead(t, file)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	evidence := recordedEvidence(t)
	for name, body := range files {
		if !strings.HasPrefix(name, "events/") || strings.Contains(body, "\nsources:") {
			continue
		}
		resource := "github://fixture/sources/" + name
		files[name] = strings.Replace(body, "\n---\n", "\nsources:\n  - resource: "+resource+"\n---\n", 1)
		evidence = append(evidence, curator.Evidence{ID: resource, Location: resource, Revision: strings.Repeat("a", 40),
			Digest: publication.Digest([]byte(body)), Classification: "policy", Content: body})
	}
	files["large-reference.txt"] = strings.Repeat("private corpus fixture marker\n", 12_000)
	forge := &lostResponseForge{Memory: publish.NewMemory(strings.Repeat("f", 40), files)}
	identity, _, err := forge.Resolve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = admin.CreateWorkspace(ctx, postgres.Workspace{ID: ws, DisplayName: ws, KnowledgeRepo: identity.Repository}); err != nil {
		t.Fatal(err)
	}
	suite, err := evaluations.Load(filepath.Join(root, "examples/banking/tests/cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range suite {
		suite[i].Critical = suite[i].ID == "topic-name"
	}
	raw, err := json.Marshal(suite)
	if err != nil {
		t.Fatal(err)
	}
	cases := filepath.Join(t.TempDir(), "cases.json")
	if err = os.WriteFile(cases, raw, 0600); err != nil {
		t.Fatal(err)
	}
	evidence[0].Content += "\nprivate-case-evidence-fixture-marker"
	ix := index.New(app)
	release := &publication.Service{DB: app, Index: ix, Workspace: ws, Forge: forge, Identity: identity,
		CasesPath: cases, PolicyDigest: "fixture-policy", Sources: func(context.Context) (publication.EvidenceSet, error) { return publication.EvidenceSet(evidence), nil }}
	if err = app.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE workspaces SET publication_hold=false WHERE id=$1`, ws)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = release.RequestRelease(ctx, publication.Request{Trigger: "bootstrap"}); err != nil {
		t.Fatal(err)
	}
	if _, err = release.Process(ctx); err != nil {
		t.Fatal(err)
	}
	gapStore := gaps.NewService(app)
	const secretSentinel = "synthetic-trace-authorization-24"
	const piiSentinel = "kf24-fixture@example.test"
	tracePath, err := filepath.EvalSymlinks(filepath.Join(root, "examples/banking/tests/trace.json"))
	if err != nil {
		t.Fatal(err)
	}
	traceInput, err := traces.ReadLocal(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	traceInput = bytes.Replace(traceInput, []byte("Please check"), []byte("Contact "+piiSentinel+". Please check"), 1)
	traceInput = bytes.Replace(traceInput, []byte("The guide currently"), []byte("Authorization: Bearer "+secretSentinel+". The guide currently"), 1)
	who := auth.Principal{WorkspaceID: ws, ActorID: "fixture", Role: auth.RoleAgent}
	traceRef, err := (traces.Store{DB: app}).Import(ctx, auth.Principal{WorkspaceID: ws, ActorID: "operator", Role: auth.RoleCurator},
		"primary", who.ActorID, traceInput)
	if err != nil {
		t.Fatal(err)
	}
	gap, err := gapStore.ReportAs(ctx, domain.GapReport{WorkspaceID: ws, AgentRunID: "primary", TaskKind: "integration-testing",
		Category: domain.CategoryStaleInstruction, Summary: "Dedupe guide uses the wrong key",
		Observed: "The guide identifies an incorrect key", Expected: "The event contract uses eventId",
		KnowledgeRefs: []string{"kb://banking/events/movement-lifecycle"}, ClientEventID: "primary", TraceRef: &traceRef}, "fixture", who)
	if err != nil {
		t.Fatal(err)
	}
	other, err := gapStore.Report(ctx, domain.GapReport{WorkspaceID: ws, AgentRunID: "unrelated", TaskKind: "integration-testing",
		Category: domain.CategoryMissingProcedure, Summary: "A separate procedure is undocumented", Observed: "a", Expected: "b", ClientEventID: "unrelated"}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err = app.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO gap_retrieval_cases(workspace_id,gap_id,case_id) VALUES ($1,$2,'mov-dedupe-key')`, ws, gap.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "activities.log")
	activityLogs, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer activityLogs.Close()
	logger := log.NewStructuredLogger(slog.New(slog.NewTextHandler(activityLogs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	c, err := client.Dial(client.Options{HostPort: address, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	store := investigation.Store{DB: app, Workspace: ws, PolicyVersion: "test", MaxConcurrent: 1}
	starter := workflows.AttemptStarter{Client: c, Store: store, Queue: workflows.AttemptQueue("acceptance", ws)}
	var attempts [2]investigation.Input
	var claimErrors [2]error
	var claims sync.WaitGroup
	for i := range attempts {
		claims.Go(func() { attempts[i], claimErrors[i] = store.Enqueue(ctx, gap.ID) })
	}
	claims.Wait()
	if claimErrors[0] != nil || claimErrors[1] != nil || attempts[0].AttemptID != attempts[1].AttemptID {
		t.Fatalf("duplicate claims: %+v %+v", attempts, claimErrors)
	}
	activities := &workflows.InvestigationActivities{Store: store, Starter: starter, Steps: workflows.Steps{
		Workspace: ws, Gaps: gapStore, DB: app, Index: ix, Publication: release, Publisher: forge,
		Rules: []curator.Rule{curator.BankingDedupeRule()}, Evidence: evidence}}
	newWorker := func() worker.Worker {
		w := worker.New(c, starter.Queue, worker.Options{})
		w.RegisterWorkflow(workflows.InvestigateGapWorkflowV2)
		w.RegisterWorkflow(workflows.DrainInvestigationsWorkflowV2)
		w.RegisterActivity(activities)
		if err := w.Start(); err != nil {
			t.Fatal(err)
		}
		return w
	}
	first := newWorker()
	t.Cleanup(func() {
		if first != nil {
			first.Stop()
		}
	})
	drain, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{TaskQueue: starter.Queue}, workflows.DrainInvestigationsWorkflowV2,
		workflows.DrainInvestigationsInput{WorkspaceID: ws})
	if err != nil {
		t.Fatal(err)
	}
	if err = drain.Get(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var in investigation.Input
	await23(ctx, t, func() bool {
		in, err = store.Review(ctx, gap.ID)
		return err == nil
	})
	if _, err := store.Get(ctx, investigation.Input{WorkspaceID: "other", GapID: in.GapID, AttemptID: in.AttemptID,
		BaseReleaseID: in.BaseReleaseID, PolicyVersion: in.PolicyVersion}); !errors.Is(err, investigation.ErrMismatch) {
		t.Fatalf("workspace mismatch %v", err)
	}
	ageReview := func(days int) {
		t.Helper()
		if err := app.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE investigation_attempts SET review_at=now()-make_interval(days=>$3)
                WHERE (workspace_id,id)=($1,$2)`, ws, in.AttemptID, days)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	ageReview(8)
	if err := c.SignalWorkflow(ctx, investigation.WorkflowID(in), "", workflows.SignalAttemptReview, false); err != nil {
		t.Fatal(err)
	}
	await23(ctx, t, func() bool {
		reviewed, err := store.ReviewClock(ctx, in)
		return err == nil && reviewed.Reminded && reviewed.State == "review"
	})
	observed, err := forge.PullRequest(ctx, 1)
	if err != nil || observed.State != "open" || observed.MergeSHA != "" {
		t.Fatalf("reminder changed PR %+v %v", observed, err)
	}
	forge.unavailable.Store(true)
	ageReview(31)
	for _, step := range []string{"expiry", "parked recheck"} {
		review, err := activities.CheckInvestigationReview(ctx, in, false)
		if err != nil || review.State != "parked" {
			t.Fatalf("%s with unavailable forge: %+v %v", step, review, err)
		}
	}
	forge.unavailable.Store(false)
	resumed, err := store.Resume(ctx, in.AttemptID)
	if err != nil || resumed != in {
		t.Fatalf("resume %+v %v", resumed, err)
	}
	if err = c.SignalWorkflow(ctx, investigation.WorkflowID(in), "", workflows.SignalResume, true); err != nil {
		t.Fatal(err)
	}
	first.Stop()
	first = nil
	second := newWorker()
	stopped := false
	defer func() {
		if !stopped {
			second.Stop()
		}
	}()
	pr, err := forge.PullRequest(ctx, 1)
	if err != nil || pr.Branch != "curator/"+in.AttemptID {
		t.Fatalf("PR %+v %v", pr, err)
	}
	if _, err := forge.PullRequest(ctx, 2); err == nil {
		t.Fatal("duplicate PR")
	}
	if _, err = forge.HumanMerge(1); err != nil {
		t.Fatal(err)
	}
	workflowID := investigation.WorkflowID(in)
	if err = c.SignalWorkflow(ctx, workflowID, "", workflows.SignalAttemptReview, false); err != nil {
		t.Fatal(err)
	}
	if err = c.GetWorkflow(ctx, workflowID, "").Get(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = release.Process(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := gapStore.Status(ctx, ws, gap.ID)
	if err != nil || current.State != domain.StateVerified {
		t.Fatalf("curated gap %+v %v", current, err)
	}
	current, err = gapStore.Status(ctx, ws, other.ID)
	if err != nil || current.State != domain.StateNew {
		t.Fatalf("unrelated gap %+v %v", current, err)
	}
	replay23(ctx, t, c, workflowID, workflows.InvestigateGapWorkflowV2,
		"private-case-evidence-fixture-marker", secretSentinel, piiSentinel, traceRef)
	second.Stop()
	stopped = true
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		var outcome, payload, proposal string
		if err := tx.QueryRow(ctx, `SELECT trace_outcome FROM investigation_attempts WHERE id=$1`, in.AttemptID).Scan(&outcome); err != nil {
			return err
		}
		if outcome != "used" {
			t.Fatalf("canonical activity trace outcome %q", outcome)
		}
		if err := tx.QueryRow(ctx, `SELECT sanitised_payload::text FROM trace_records`).Scan(&payload); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT to_jsonb(p)::text FROM proposals p WHERE id=$1`, in.AttemptID).Scan(&proposal); err != nil {
			return err
		}
		for _, sink := range []string{payload, proposal, string(logged)} {
			for _, sentinel := range []string{secretSentinel, piiSentinel} {
				if strings.Contains(sink, sentinel) {
					t.Fatal("raw trace sentinel escaped ingestion")
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func await23(ctx context.Context, t *testing.T, ok func() bool) {
	t.Helper()
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()
	for !ok() {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func replay23(ctx context.Context, t *testing.T, c client.Client, id string, fn any, forbidden ...string) {
	t.Helper()
	iterator := c.GetWorkflowHistory(ctx, id, "", false, 0)
	history := &historypb.History{}
	for iterator.HasNext() {
		event, err := iterator.Next()
		if err != nil {
			t.Fatal(err)
		}
		history.Events = append(history.Events, event)
	}
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(fn)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range append(forbidden, "private corpus fixture marker") {
		if value != "" && bytes.Contains(raw, []byte(value)) {
			t.Fatal("private source material present in workflow history")
		}
	}
	if len(raw) > 64<<10 {
		t.Fatalf("workflow history size %d", len(raw))
	}
}
