package publication

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
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
		"note.md":  "---\ntype: Note\ntitle: Pipeline protocol\nstatus: stable\nsources:\n  - resource: github://fixture/sources/policy.md\n---\n\nThe pipeline protocol marker is teal.\n",
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
		Sources: func(context.Context) (EvidenceSet, error) {
			return EvidenceSet{{ID: "policy", Location: "github://fixture/sources/policy.md", Revision: strings.Repeat("b", 40), Digest: "fixture", Classification: "policy", Content: "The pipeline protocol marker is teal."}}, nil
		}}
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
	if err := app.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE publication_operations SET lease_until=now()-interval '1 second' WHERE workspace_id=$1`, ws)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	continued, err := svc.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := svc.evaluate(ctx, continued, "", base, receipt.SHA, files)
	if err != nil || restored.CandidateID != receipt.CandidateID {
		t.Fatalf("stage recovery %+v %v", restored, err)
	}
	if _, err := svc.Promote(ctx, receipt.CandidateID, base.Generation); !errors.Is(err, ErrStale) {
		t.Fatalf("previous lease installed a recovered stage: %v", err)
	}
	if _, err := svc.promote(ctx, continued, restored.CandidateID, base.Generation); err != nil {
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

func Test22ExactMergedPRAndRecoveryAtCurrentHead(t *testing.T) {
	svc, base, files := publicationFixture(t)
	ctx := context.Background()
	forge := svc.Forge.(*publish.Memory)
	notes := []string{"alpha.md", "beta.md", "gamma.md"}
	ids := make([]string, len(notes))
	merges := make([]string, len(notes))
	for i, name := range notes {
		id := uuid.NewString()
		ids[i] = id
		prop := curator.Proposal{
			Decision:        "update",
			Edits:           []curator.Edit{{Path: name, Old: "`teal`", New: "`blue`"}},
			Claims:          []curator.Claim{{Claim: "Update the protocol marker", EvidenceIDs: []string{"policy"}}},
			SourceRevisions: []curator.Source{{Location: "github://fixture/sources/policy.md", Revision: strings.Repeat("b", 40), Digest: "test-digest"}},
		}
		// Create all proposals from the same approved base. Git can later merge
		// each independently because none of the updates overlap.
		pr, err := forge.CreatePullRequest(ctx, publish.Request{Branch: "curator/" + id, Files: map[string]string{
			name: strings.ReplaceAll(files[name], "`teal`", "`blue`"),
		}})
		if err != nil {
			t.Fatal(err)
		}
		seedProposal(t, svc, id, base.SHA, prop, pr)
	}
	svc.Forge = retargetedForge{Memory: forge}
	for _, i := range []int{0, 2} {
		var err error
		merges[i], err = forge.HumanMerge(i + 1)
		if err != nil {
			t.Fatal(err)
		}
	}
	final := forge.Files()
	final["gamma.md"] = files["gamma.md"]
	head := forge.HumanCommit(final)
	opID, err := svc.RequestRelease(ctx, Request{Trigger: "pr-merge", PRNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := svc.claim(ctx)
	if err != nil || claim.id != opID {
		t.Fatalf("claim %+v %v", claim, err)
	}
	receipt, err := svc.evaluate(ctx, claim, "", base, head, final)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Promote(ctx, receipt.CandidateID, base.Generation); err != nil {
		t.Fatal(err)
	}
	// Model a process stopping after pointer commit and before gap reconciliation.
	if _, err := svc.RequestRelease(ctx, Request{Trigger: "reconcile"}); err != nil {
		t.Fatal(err)
	}
	if done, err := svc.Process(ctx); !done || err != nil {
		t.Fatalf("recover %t %v", done, err)
	}
	if err := svc.DB.WithWorkspace(ctx, svc.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		for i, id := range ids {
			want := "awaiting-review"
			if i == 0 {
				want = "published"
			}
			var state string
			if err := tx.QueryRow(ctx, "SELECT state FROM gap_reports WHERE id=$1", id).Scan(&state); err != nil {
				return err
			}
			if state != want {
				t.Errorf("%s gap state %q, want %q", notes[i], state, want)
			}
		}
		var merge string
		if err := tx.QueryRow(ctx, "SELECT merge_sha FROM proposal_merge_links WHERE pr_number=1").Scan(&merge); err != nil {
			return err
		}
		if merge != merges[0] || merge == head {
			t.Errorf("merge link %q for head %q", merge, head)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	current, err := svc.Baseline(ctx)
	if err != nil || current.Generation != base.Generation+1 {
		t.Fatalf("reconciliation moved the pointer again: %+v %v", current, err)
	}
}

func Test22HeadRecheckAndConcurrentGenerationCAS(t *testing.T) {
	svc, base, files := publicationFixture(t)
	ctx := context.Background()
	forge := svc.Forge.(*publish.Memory)
	stage := func(head string) Receipt {
		t.Helper()
		if _, err := svc.RequestRelease(ctx, Request{Trigger: "operator"}); err != nil {
			t.Fatal(err)
		}
		op, err := svc.claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := svc.evaluate(ctx, op, "", base, head, files)
		if err != nil {
			t.Fatal(err)
		}
		return receipt
	}
	firstHead := forge.HumanCommit(files)
	stale := stage(firstHead)
	secondHead := forge.HumanCommit(files)
	if _, err := svc.Promote(ctx, stale.CandidateID, base.Generation); !errors.Is(err, publish.ErrStaleBase) {
		t.Fatalf("stale head: %v", err)
	}
	a, b := stage(secondHead), stage(secondHead)
	errorsFound := make(chan error, 2)
	for _, candidate := range []Receipt{a, b} {
		go func() {
			_, err := svc.Promote(ctx, candidate.CandidateID, base.Generation)
			errorsFound <- err
		}()
	}
	left, right := <-errorsFound, <-errorsFound
	if right == nil {
		left, right = right, left
	}
	if left != nil || !errors.Is(right, ErrStale) {
		t.Fatalf("concurrent promotions: %v %v", left, right)
	}
}

func publicationFixture(t *testing.T) (*Service, Baseline, map[string]string) {
	t.Helper()
	ctx := context.Background()
	admin, app := testutil.OpenMigrated(t)
	ws := "release-" + uuid.NewString()
	files := map[string]string{
		"index.md": "---\nokf_version: \"0.3\"\nx-akf-uri-prefix: kb://release\n---\n# Release\n",
	}
	for _, name := range []string{"alpha.md", "beta.md", "gamma.md"} {
		files[name] = "---\ntype: Note\ntitle: Pipeline protocol\nstatus: stable\nsources:\n  - resource: github://fixture/sources/policy.md\n---\n\nThe pipeline protocol marker is `teal`.\n"
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
		suite[i] = evaluations.Case{ID: uuid.NewString(), Kind: "documentation-gap", RetrievalQuery: "pipeline protocol", Answer: "protocol", Extract: "contains", Critical: true}
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
		Sources: func(context.Context) (EvidenceSet, error) {
			return EvidenceSet{{ID: "policy", Location: "github://fixture/sources/policy.md", Revision: strings.Repeat("b", 40),
				Digest: "test-digest", Classification: "policy", Content: "The pipeline protocol marker is `blue`."}}, nil
		}}
	if err := app.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE workspaces SET publication_hold=false WHERE id=$1`, ws)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestRelease(ctx, Request{Trigger: "bootstrap"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Process(ctx); err != nil {
		t.Fatal(err)
	}
	base, err := svc.Baseline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return svc, base, files
}

func seedProposal(t *testing.T, svc *Service, id, base string, prop curator.Proposal, pr publish.Result) {
	t.Helper()
	edits, _ := json.Marshal(prop.Edits)
	claims, _ := json.Marshal(prop.Claims)
	sources, _ := json.Marshal(prop.SourceRevisions)
	ctx := context.Background()
	if err := svc.DB.WithWorkspace(ctx, svc.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO gap_reports(id,workspace_id,agent_id,task_kind,category,summary,observed,expected,
            client_event_id,state,dedupe_signature,failure_signature)
            VALUES($1,$2,'fixture','test','stale-instruction','test','test','test',$1::uuid::text,'awaiting-review',$1::uuid::text,$1::uuid::text)`,
			id, svc.Workspace); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO proposals(id,workspace_id,gap_ids,decision,risk_level,approval_status,edits,claim_evidence_map,source_revisions)
            VALUES($1,$2,ARRAY[$1]::uuid[],'update','low','draft',$3,$4,$5)`, id, svc.Workspace, edits, claims, sources)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordMapping(ctx, id, base, pr); err != nil {
		t.Fatal(err)
	}
}

func Test22AllReleaseTriggersRejectMissingSuite(t *testing.T) {
	svc, base, files := publicationFixture(t)
	ctx := context.Background()
	forge := svc.Forge.(*publish.Memory)
	id := uuid.NewString()
	pr, err := forge.CreatePullRequest(ctx, publish.Request{Branch: "curator/" + id, Files: map[string]string{"alpha.md": files["alpha.md"] + "\n"}})
	if err != nil {
		t.Fatal(err)
	}
	seedProposal(t, svc, id, base.SHA, curator.Proposal{}, pr)
	forge.HumanCommit(files)
	svc.CasesPath = filepath.Join(t.TempDir(), "missing.json")
	for _, request := range []Request{
		{Trigger: "push", DeliveryID: "push"}, {Trigger: "pr-merge", GapID: id}, {Trigger: "pr-head", PRNumber: pr.Number},
		{Trigger: "reconcile"}, {Trigger: "bootstrap"}, {Trigger: "operator"},
	} {
		if _, err := svc.RequestRelease(ctx, request); err != nil {
			t.Fatal(err)
		}
		if done, err := svc.Process(ctx); !done || !errors.Is(err, ErrRejected) {
			t.Fatalf("%s: processed=%t err=%v", request.Trigger, done, err)
		}
	}
	current, err := svc.Baseline(ctx)
	if err != nil || base != current {
		t.Fatalf("active release changed: before=%+v after=%+v err=%v", base, current, err)
	}
}

func Test22CandidateRankingAndDeletionUseActualIndex(t *testing.T) {
	svc, base, files := publicationFixture(t)
	cases, err := evaluations.Load(svc.CasesPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := range cases {
		cases[i].Answer = "teal"
	}
	raw, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svc.CasesPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ranked := maps.Clone(files)
	for _, path := range []string{"a1.md", "a2.md", "a3.md"} {
		ranked[path] = strings.ReplaceAll(files["alpha.md"], "teal", "blue")
	}
	receipt, err := svc.EvaluateCandidate(ctx, "", base.SHA, ranked)
	if !errors.Is(err, ErrRejected) || receipt.Reason != "critical-retrieval" || receipt.Before.Failed > 0 || receipt.After.Failed == 0 {
		t.Fatalf("ranking receipt %+v %v", receipt, err)
	}
	for _, path := range []string{"alpha.md", "beta.md", "gamma.md"} {
		delete(ranked, path)
	}
	receipt, err = svc.EvaluateCandidate(ctx, "", base.SHA, ranked)
	if !errors.Is(err, ErrRejected) || receipt.Reason != "critical-retrieval" {
		t.Fatalf("deletion receipt %+v %v", receipt, err)
	}
	item, err := svc.Index.Get(ctx, svc.Workspace, "kb://release/alpha", "")
	if err != nil || !strings.Contains(item.Body, "teal") || item.Revision != base.SHA {
		t.Fatalf("active document changed %+v %v", item, err)
	}
}

func Test22DivergentDefaultHistoryIsRejected(t *testing.T) {
	svc, approved, files := publicationFixture(t)
	ctx := context.Background()
	svc.Forge = publish.NewMemory(strings.Repeat("d", 40), files)
	if _, err := svc.RequestRelease(ctx, Request{Trigger: "reconcile"}); err != nil {
		t.Fatal(err)
	}
	if done, err := svc.Process(ctx); !done || !errors.Is(err, ErrRejected) {
		t.Fatalf("divergent history: processed=%t err=%v", done, err)
	}
	current, err := svc.Baseline(ctx)
	if err != nil || current != approved {
		t.Fatalf("publication changed on divergence: %+v %v", current, err)
	}
}

type retargetedForge struct{ *publish.Memory }

func (f retargetedForge) PullRequest(ctx context.Context, number int) (publish.PullRequest, error) {
	if number == 2 {
		return publish.PullRequest{}, publish.ErrPRMismatch
	}
	return f.Memory.PullRequest(ctx, number)
}

type countedTreeForge struct {
	*publish.Memory
	trees int
}

func (f *countedTreeForge) Tree(ctx context.Context, sha string) (map[string]string, error) {
	f.trees++
	return f.Memory.Tree(ctx, sha)
}

func TestReviewRevalidatesCurrentSHAAndSkipsIdleTrees(t *testing.T) {
	svc, base, files := publicationFixture(t)
	ctx := context.Background()
	forge := &countedTreeForge{Memory: svc.Forge.(*publish.Memory)}
	svc.Forge = forge
	run := func() error {
		t.Helper()
		if _, err := svc.RequestRelease(ctx, Request{Trigger: "reconcile"}); err != nil {
			t.Fatal(err)
		}
		_, err := svc.Process(ctx)
		return err
	}
	if err := run(); err != nil || forge.trees != 0 {
		t.Fatalf("idle trees=%d err=%v", forge.trees, err)
	}
	uncited := maps.Clone(files)
	uncited["alpha.md"] = strings.ReplaceAll(files["alpha.md"], "sources:\n  - resource: github://fixture/sources/policy.md\n", "")
	receipt, err := svc.evaluate(ctx, operation{}, "", Baseline{}, base.SHA, uncited)
	if !errors.Is(err, ErrRejected) || receipt.Reason != "claim-evidence" {
		t.Fatalf("bootstrap receipt=%+v err=%v", receipt, err)
	}
	svc.Index.Hybrid = true
	if err := run(); err != nil {
		t.Fatal(err)
	}
	current, err := svc.Baseline(ctx)
	if err != nil || current.Generation != base.Generation+1 || current.ReleaseID != base.ReleaseID {
		t.Fatalf("new evaluator %+v %v", current, err)
	}
	suites, err := evaluations.Load(svc.CasesPath)
	if err != nil {
		t.Fatal(err)
	}
	suites[0].Answer = "an impossible answer"
	raw, err := json.Marshal(suites)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svc.CasesPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(); !errors.Is(err, ErrRejected) {
		t.Fatalf("new suite bypassed: %v", err)
	}
	actual, err := svc.Baseline(ctx)
	if err != nil || actual != current {
		t.Fatalf("rejected suite changed release %+v %v", actual, err)
	}
}
