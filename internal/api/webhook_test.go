package api_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/api"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/auth"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
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
