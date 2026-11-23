package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/api"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/auth"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
)

type readyFunc func(context.Context) error

func (f readyFunc) Ping(ctx context.Context) error { return f(ctx) }

type spaceFunc func(context.Context, string) (postgres.Workspace, error)

func (f spaceFunc) GetWorkspace(ctx context.Context, id string) (postgres.Workspace, error) {
	return f(ctx, id)
}

func TestHealthAndWorkspaceScope(t *testing.T) {
	authn, err := auth.New([]config.Token{
		{Token: "agent-secret", WorkspaceID: "banking-demo", Role: auth.RoleAgent},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(nil, readyFunc(func(context.Context) error { return nil }), spaceFunc(func(_ context.Context, id string) (postgres.Workspace, error) {
		if id != "banking-demo" {
			t.Fatalf("store was queried for %s", id)
		}
		return postgres.Workspace{ID: id, DisplayName: "Banking", KnowledgeRepo: "knowledge-factory-demo-knowledge", RetentionDays: 30}, nil
	}), authn)
	handler := srv.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/workspace?workspace_id=other-demo", nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-workspace status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/workspace", nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("workspace status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["workspace_id"] != "banking-demo" {
		t.Fatalf("body = %v", body)
	}
}

func TestReadyzFailure(t *testing.T) {
	srv := api.New(nil, readyFunc(func(context.Context) error { return errors.New("down") }), nil, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
}
