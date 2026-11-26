package publication

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/evaluations"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/testutil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func Test22DurableLeaseHoldAndCandidateIsolation(t *testing.T) {
	admin, app := testutil.OpenMigrated(t)
	ctx := context.Background()
	ws := "release-" + uuid.NewString()
	files := map[string]string{
		"index.md": "---\nokf_version: \"0.3\"\nx-akf-uri-prefix: kb://release\n---\n# Release\n",
		"note.md":  "---\ntype: Note\ntitle: Pipeline protocol\nstatus: stable\n---\n\nThe pipeline protocol marker is teal.\n",
	}
	forge := publish.NewMemory(strings.Repeat("a", 40), files)
	identity, _, err := forge.Resolve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.CreateWorkspace(ctx, postgres.Workspace{ID: ws, DisplayName: ws, KnowledgeRepo: identity.Repository}); err != nil {
		t.Fatal(err)
	}
	suite := make([]evaluations.Case, 20)
	for i := range suite {
		suite[i] = evaluations.Case{ID: uuid.NewString(), Kind: "documentation-gap", RetrievalQuery: "pipeline protocol", Answer: "teal", Extract: "contains", Critical: true}
	}
	raw, err := json.Marshal(suite)
	if err != nil {
		t.Fatal(err)
	}
	cases := filepath.Join(t.TempDir(), "suite.json")
	if err = os.WriteFile(cases, raw, 0600); err != nil {
		t.Fatal(err)
	}
	svc := &Service{DB: app, Index: index.New(app), Forge: forge, Workspace: ws, Identity: identity, CasesPath: cases, PolicyDigest: "policy",
		Sources: func(context.Context) (EvidenceSet, error) { return nil, nil }}
	id, err := svc.RequestRelease(ctx, Request{Trigger: "push", DeliveryID: "22-F"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.RequestRelease(ctx, Request{Trigger: "push", DeliveryID: "22-F"})
	if err != nil || id != again {
		t.Fatalf("duplicate %s %v", again, err)
	}
	old, err := svc.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE publication_operations SET lease_until=now()-interval '1 second' WHERE workspace_id=$1`, ws)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := svc.claim(ctx)
	if err != nil || claimed.token <= old.token {
		t.Fatalf("claim %+v %v", claimed, err)
	}
	if err := svc.finish(ctx, old, "rejected", "stale worker"); !errors.Is(err, ErrStale) {
		t.Fatalf("old lease: %v", err)
	}
	base, err := svc.Baseline(ctx)
	if err != nil || !base.Held {
		t.Fatalf("base %+v %v", base, err)
	}
	receipt, err := svc.evaluate(ctx, claimed, "", base, strings.Repeat("a", 40), files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Index.Get(ctx, ws, "kb://release/note", candidateKey(receipt.CandidateID)); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("candidate exposed: %v", err)
	}
	if hits, err := svc.Index.Search(ctx, ws, "pipeline protocol", "", 3); err != nil || len(hits) != 0 {
		t.Fatalf("candidate results: %+v %v", hits, err)
	}
	if _, err := svc.Promote(ctx, receipt.CandidateID, base.Generation); !errors.Is(err, ErrHeld) {
		t.Fatalf("rollout hold: %v", err)
	}
	if err := app.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE workspaces SET publication_hold=false WHERE id=$1`, ws)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Promote(ctx, receipt.CandidateID, base.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Promote(ctx, receipt.CandidateID, base.Generation); !errors.Is(err, ErrStale) {
		t.Fatalf("stale promotion: %v", err)
	}
	if err := svc.Index.Reindex(ctx, ws, "b", t.TempDir()); err == nil {
		t.Fatal("direct publication enabled")
	}
	pr, err := forge.CreatePullRequest(ctx, publish.Request{Branch: "curator/test", Files: map[string]string{"note.md": strings.ReplaceAll(files["note.md"], "teal", "blue")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := forge.HumanMerge(pr.Number); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestRelease(ctx, Request{Trigger: "operator"}); err != nil {
		t.Fatal(err)
	}
	if done, err := svc.Process(ctx); !done || !errors.Is(err, ErrRejected) {
		t.Fatalf("regression: %t %v", done, err)
	}
	base, err = svc.Baseline(ctx)
	if err != nil || base.SHA != strings.Repeat("a", 40) {
		t.Fatalf("changed after rejection: %+v %v", base, err)
	}
}
