package api_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/api"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/auth"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publication"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/testutil"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type memDeliveries struct{ ids map[string]bool }

func (m *memDeliveries) Seen(_ context.Context, id string) (bool, error) {
	return m.ids[id], nil
}

func (m *memDeliveries) Remember(_ context.Context, id string) (bool, error) {
	if m.ids[id] {
		return false, nil
	}
	m.ids[id] = true
	return true, nil
}

type memReviews struct {
	signals    int
	reconciles int
}

func (m *memReviews) Signal(context.Context, string, workflows.ReviewSignal) error {
	m.signals++
	return nil
}

func (m *memReviews) Reconcile(context.Context) error {
	m.reconciles++
	return nil
}

func TestApprovalRequiresReviewerOrCurator(t *testing.T) {
	authn, err := auth.New([]config.Token{
		{Token: "agent-token", WorkspaceID: "banking-demo", Role: "agent"},
		{Token: "review-token", WorkspaceID: "banking-demo", Role: "reviewer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	reviews := &memReviews{}
	srv := api.New(nil, nil, nil, authn)
	srv.Reviews = reviews
	body := []byte(`{"action":"reject","reason":"no"}`)
	agent := httptest.NewRequest(http.MethodPost, "/v1/gaps/gap-1/approval", bytes.NewReader(body))
	agent.Header.Set("Authorization", "Bearer agent-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, agent)
	if rec.Code != http.StatusForbidden || reviews.signals != 0 {
		t.Fatalf("agent status %d signals %d", rec.Code, reviews.signals)
	}
	reviewer := httptest.NewRequest(http.MethodPost, "/v1/gaps/gap-1/approval", bytes.NewReader(body))
	reviewer.Header.Set("Authorization", "Bearer review-token")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, reviewer)
	if rec.Code != http.StatusOK || reviews.signals != 1 {
		t.Fatalf("reviewer status %d signals %d body %s", rec.Code, reviews.signals, rec.Body.String())
	}
}

func TestReviewRequiresACaller(t *testing.T) {
	srv := api.New(nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/review", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestApprovalRequiresACaller(t *testing.T) {
	srv := api.New(nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/gaps/gap-1/approval", bytes.NewReader([]byte(`{"action":"reject"}`)))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestWebhookRejectsABadSignature(t *testing.T) {
	srv := api.New(nil, nil, nil, nil)
	srv.WebhookSecret = "top-secret"
	body := []byte(`{"action":"closed"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/github/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	req.Header.Set("X-GitHub-Delivery", "d1")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestWebhookSignalsMergeOnce(t *testing.T) {
	reviews := &memReviews{}
	srv := api.New(nil, nil, nil, nil)
	srv.WebhookSecret = "top-secret"
	srv.Deliveries = &memDeliveries{ids: map[string]bool{}}
	srv.Reviews = reviews
	body := []byte(`{"action":"closed","pull_request":{"merged":true,"merge_commit_sha":"abc","head":{"ref":"curator/gap-1","sha":"def"}}}`)
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/github/webhook", bytes.NewReader(body))
		req.Header.Set("X-Hub-Signature-256", api.Sign(srv.WebhookSecret, body))
		req.Header.Set("X-GitHub-Delivery", "d1")
		req.Header.Set("X-GitHub-Event", "pull_request")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	if post().Code != http.StatusOK || reviews.signals != 1 {
		t.Fatalf("signals %d", reviews.signals)
	}
	if post().Code != http.StatusOK || reviews.signals != 1 {
		t.Fatalf("duplicate signaled again: %d", reviews.signals)
	}
	push := []byte(`{"ref":"refs/heads/main","after":"abc"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/github/webhook", bytes.NewReader(push))
	req.Header.Set("X-Hub-Signature-256", api.Sign(srv.WebhookSecret, push))
	req.Header.Set("X-GitHub-Delivery", "d2")
	req.Header.Set("X-GitHub-Event", "push")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || reviews.reconciles != 1 {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("reconcile status %d body %s count %d", rec.Code, body, reviews.reconciles)
	}
}

type failOnceReviews struct {
	memReviews
	fail bool
}

func (f *failOnceReviews) Signal(context.Context, string, workflows.ReviewSignal) error {
	if f.fail {
		f.fail = false
		return io.EOF
	}
	f.signals++
	return nil
}

func TestWebhookRetryAfterSignalFailure(t *testing.T) {
	reviews := &failOnceReviews{fail: true}
	store := &memDeliveries{ids: map[string]bool{}}
	srv := api.New(nil, nil, nil, nil)
	srv.WebhookSecret = "top-secret"
	srv.Deliveries = store
	srv.Reviews = reviews
	body := []byte(`{"action":"closed","pull_request":{"merged":true,"merge_commit_sha":"abc","head":{"ref":"curator/gap-1","sha":"def"}}}`)
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/github/webhook", bytes.NewReader(body))
		req.Header.Set("X-Hub-Signature-256", api.Sign(srv.WebhookSecret, body))
		req.Header.Set("X-GitHub-Delivery", "retry-me")
		req.Header.Set("X-GitHub-Event", "pull_request")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	if post().Code != http.StatusBadGateway || store.ids["retry-me"] {
		t.Fatal("failed signal was recorded")
	}
	if post().Code != http.StatusOK || reviews.signals != 1 {
		t.Fatalf("retry signals=%d", reviews.signals)
	}
}

func TestWebhookReconcilesTheConfiguredBranch(t *testing.T) {
	reviews := &memReviews{}
	srv := api.New(nil, nil, nil, nil)
	srv.WebhookSecret = "top-secret"
	srv.DefaultBranch = "trunk"
	srv.Deliveries = &memDeliveries{ids: map[string]bool{}}
	srv.Reviews = reviews
	push := func(ref string) int {
		body := []byte(`{"ref":"` + ref + `","after":"abc"}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/github/webhook", bytes.NewReader(body))
		req.Header.Set("X-Hub-Signature-256", api.Sign(srv.WebhookSecret, body))
		req.Header.Set("X-GitHub-Delivery", ref)
		req.Header.Set("X-GitHub-Event", "push")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if push("refs/heads/main") != http.StatusOK || reviews.reconciles != 0 {
		t.Fatalf("main reconciled on a trunk workspace: %d", reviews.reconciles)
	}
	if push("refs/heads/trunk") != http.StatusOK || reviews.reconciles != 1 {
		t.Fatalf("trunk reconcile count %d", reviews.reconciles)
	}
}

func Test22WebhookAtomicallyEnqueuesExactDelivery(t *testing.T) {
	ctx := context.Background()
	admin, app := testutil.OpenMigrated(t)
	ws := "webhook-" + uuid.NewString()
	identity := publish.Identity{Repository: "fixture/knowledge", Installation: "42", Branch: "main"}
	if err := admin.CreateWorkspace(ctx, postgres.Workspace{ID: ws, DisplayName: ws, KnowledgeRepo: identity.Repository}); err != nil {
		t.Fatal(err)
	}
	srv := api.New(nil, nil, nil, nil)
	srv.WebhookSecret = "local-fixture"
	srv.Publication = &publication.Service{DB: app, Workspace: ws, Identity: identity}
	post := func(event, delivery, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/github/webhook", strings.NewReader(body))
		req.Header.Set("X-Hub-Signature-256", api.Sign(srv.WebhookSecret, []byte(body)))
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-GitHub-Event", event)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	push := `{"repository":{"full_name":"fixture/knowledge"},"installation":{"id":42},"ref":"refs/heads/main","after":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	if rec := post("push", "bad-repo", strings.Replace(push, "fixture/knowledge", "other/knowledge", 1)); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign repo: %d", rec.Code)
	}
	if rec := post("push", "bad-installation", strings.Replace(push, "42", "12", 1)); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign installation: %d", rec.Code)
	}
	pr := `{"repository":{"full_name":"fixture/knowledge"},"installation":{"id":42},"number":2,"action":"closed","pull_request":{"base":{"ref":"main"}}}`
	if rec := post("pull_request", "unmapped", pr); rec.Code != http.StatusNotFound {
		t.Fatalf("unmapped PR: %d", rec.Code)
	}
	first, duplicate := post("push", "same", push), post("push", "same", push)
	if first.Code != http.StatusAccepted || duplicate.Code != first.Code || duplicate.Body.String() != first.Body.String() {
		t.Fatalf("delivery response %d %s %d %s", first.Code, first.Body, duplicate.Code, duplicate.Body)
	}
	if err := app.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		err := tx.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries`).Scan(&n)
		if err == nil && n != 1 {
			t.Errorf("durable deliveries %d", n)
		}
		err = tx.QueryRow(ctx, `SELECT count(*) FROM publication_operations WHERE state='queued'`).Scan(&n)
		if err == nil && n != 1 {
			t.Errorf("durable operations %d", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

type legacyCloseReviews struct {
	memReviews
	number int
	branch string
}

func (m *legacyCloseReviews) SignalLegacyClose(_ context.Context, number int, branch string) (bool, error) {
	m.number, m.branch = number, branch
	return true, nil
}

func TestPublicationWebhookDeliversLegacyClose(t *testing.T) {
	srv := api.New(nil, nil, nil, nil)
	srv.WebhookSecret = "fixture-secret"
	srv.Publication = &publication.Service{Identity: publish.Identity{Repository: "fixture/knowledge", Installation: "42", Branch: "main"}}
	reviews := &legacyCloseReviews{}
	srv.Reviews = reviews
	body := []byte(`{"repository":{"full_name":"fixture/knowledge"},"installation":{"id":42},"number":7,"action":"closed","pull_request":{"merged":false,"base":{"ref":"main"},"head":{"ref":"curator/fixture"}}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/github/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", api.Sign(srv.WebhookSecret, body))
	req.Header.Set("X-GitHub-Delivery", "legacy-close")
	req.Header.Set("X-GitHub-Event", "pull_request")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || reviews.number != 7 || reviews.branch != "curator/fixture" {
		t.Fatalf("legacy close %d %+v", rec.Code, reviews)
	}
}

func TestReviewerViewsShowReceiptWithoutCandidatePayload(t *testing.T) {
	ctx := context.Background()
	admin, app := testutil.OpenMigrated(t)
	ws := "reviews-" + uuid.NewString()
	if err := admin.CreateWorkspace(ctx, postgres.Workspace{ID: ws, DisplayName: ws, KnowledgeRepo: "fixture/knowledge"}); err != nil {
		t.Fatal(err)
	}
	candidate := uuid.NewString()
	if err := app.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO publication_receipts(workspace_id,candidate_id,lease_token,baseline_sha,generation,git_sha,
		    tree_digest,policy_digest,dataset_digest,evaluator_digest,outcome,reason,results)
		    VALUES($1,$2,0,'',0,'new','tree','policy','dataset','eval','rejected','claim-evidence',
		      '[{},{"Passed":4,"Failed":2,"CriticalRegressions":1,"Results":[{"Detail":"private fixture text"}]}]')`, ws, candidate)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	authn, err := auth.New([]config.Token{{Token: "fixture-reviewer", WorkspaceID: ws, Role: "reviewer"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(nil, nil, nil, authn)
	srv.DB = app
	for _, path := range []string{"/v1/evaluations", "/v1/review"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer fixture-reviewer")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(body, "claim-evidence") || !strings.Contains(body, `"passed":4`) ||
			strings.Contains(body, candidate) || strings.Contains(body, "private fixture text") {
			t.Fatalf("%s returned %d %s", path, rec.Code, body)
		}
	}
}
