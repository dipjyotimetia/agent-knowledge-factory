package investigation

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/testutil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func Test23InterleavedEvidenceAndExpiredLease(t *testing.T) {
	admin, app := testutil.OpenMigrated(t)
	ctx := context.Background()
	ws := "lease-" + uuid.NewString()
	if err := admin.CreateWorkspace(ctx, postgres.Workspace{ID: ws, DisplayName: ws, KnowledgeRepo: "fixture/knowledge"}); err != nil {
		t.Fatal(err)
	}
	store := Store{DB: app, Workspace: ws, PolicyVersion: "test"}
	base := uuid.NewString()
	if err := app.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO publication_receipts(workspace_id,candidate_id,lease_token,baseline_sha,generation,git_sha,
            tree_digest,policy_digest,dataset_digest,evaluator_digest,outcome,reason,results)
            VALUES($1,$2,0,'',0,'base','tree','policy','dataset','evaluator','passed','fixture','[]')`, ws, base); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO knowledge_releases(workspace_id,id,repository,git_sha,tree_digest,policy_digest,dataset_digest,receipt_id)
            VALUES($1,$2,'fixture/knowledge','base','tree','policy','dataset',$2)`, ws, base); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE workspaces SET active_release_id=$2,publication_hold=false WHERE id=$1`, ws, base)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	gapStore := gaps.NewService(app)
	for _, item := range []string{"alpha", "beta", "gamma", "delta", "epsilon"} {
		g, err := gapStore.Report(ctx, domain.GapReport{WorkspaceID: ws, AgentRunID: item, TaskKind: "test", Category: domain.CategoryMissingProcedure,
			Summary: "Need a procedure for " + item, Observed: "Documentation missing for " + item, Expected: "Procedure for " + item,
			KnowledgeRefs: []string{"kb://fixture/" + item}, ClientEventID: item}, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Enqueue(ctx, g.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := gapStore.Feedback(ctx, domain.Feedback{WorkspaceID: ws, KnowledgeURI: "kb://fixture/" + item, Revision: "base", Observation: item}); err != nil {
			t.Fatal(err)
		}
	}
	inputs, err := store.Starts(ctx)
	if err != nil || len(inputs) != 4 {
		t.Fatalf("capacity %d %v", len(inputs), err)
	}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		calls.Go(func() {
			in := inputs[i]
			lease, err := store.Acquire(ctx, in, "prepare", "input")
			if err != nil {
				errs[i] = err
				entered <- struct{}{}
				return
			}
			leased := store.Fence(ctx, lease)
			ev := curator.Evidence{ID: "document", Location: "github://fixture/source/" + in.GapID, Revision: "pin", Digest: in.GapID, Classification: "contract", Content: in.GapID}
			rows, err := store.Evidence(leased, in, func(context.Context) ([]curator.Evidence, error) {
				entered <- struct{}{}
				<-release
				return []curator.Evidence{ev}, nil
			})
			if err != nil || len(rows) != 1 || rows[0] != ev {
				errs[i] = errors.New("wrong attempt evidence")
				return
			}
			rows, err = store.Evidence(leased, in, func(context.Context) ([]curator.Evidence, error) { return nil, errors.New("refetched") })
			if err != nil || len(rows) != 1 || rows[0] != ev {
				errs[i] = errors.New("evidence changed on retry")
				return
			}
			feedback, err := store.Feedback(leased, in)
			if err != nil || len(feedback) != 1 {
				errs[i] = errors.New("attempt feedback scope mismatch")
			}
		})
	}
	<-entered
	<-entered
	close(release)
	calls.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("interleaved attempts: %v", errs)
	}
	in := inputs[0]
	lease := Lease{Input: in, Kind: "prepare", Token: 1}
	leased := store.Fence(ctx, lease)
	if err := store.ReserveModel(leased, in, "model-request", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveModel(leased, in, "model-request", 1); !errors.Is(err, ErrBudget) {
		t.Fatalf("model budget: %v", err)
	}
	original := curator.Proposal{Decision: "no-change"}
	if err := store.CompleteModel(leased, in, "model-request", original); err != nil {
		t.Fatal(err)
	}
	if prop, ok, err := store.ModelResult(leased, in, "model-request"); err != nil || !ok || prop.Decision != original.Decision {
		t.Fatalf("cached model result: %+v %t %v", prop, ok, err)
	}
	if err := app.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE operation_ledger SET lease_until=now()-interval '1 second' WHERE (workspace_id,attempt_id)=($1,$2)`, ws, in.AttemptID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	replaced, err := store.Acquire(ctx, in, "prepare", "input")
	if err != nil || replaced.Token < 2 {
		t.Fatalf("lease %+v %v", replaced, err)
	}
	stale := replaced
	stale.Token--
	if err = store.Finish(ctx, stale, "completed", "stale", ""); !errors.Is(err, ErrStale) {
		t.Fatalf("wrote with stale token: %v", err)
	}
	if err = store.End(ctx, in, "cancelled"); err != nil {
		t.Fatal(err)
	}
	if err = store.Finish(ctx, replaced, "completed", "late", ""); !errors.Is(err, ErrStale) {
		t.Fatalf("wrote after cancellation: %v", err)
	}
	gap, err := gapStore.Status(ctx, ws, in.GapID)
	if err != nil || gap.State != domain.StateCancelled {
		t.Fatalf("gap cancellation %+v %v", gap, err)
	}
	inputs, err = store.Starts(ctx)
	if err != nil || len(inputs) != 4 {
		t.Fatalf("queue did not refill %d %v", len(inputs), err)
	}
}
