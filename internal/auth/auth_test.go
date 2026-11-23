package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
)

func TestAuthenticatorBindsWorkspace(t *testing.T) {
	a, err := New([]config.Token{
		{Token: "agent-secret", WorkspaceID: "banking-demo", Role: RoleAgent},
		{Token: "other-secret", WorkspaceID: "other-demo", Role: RoleAgent},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := a.Authenticate("agent-secret")
	if !ok || got.WorkspaceID != "banking-demo" || got.Role != RoleAgent {
		t.Fatalf("principal = %+v ok=%v", got, ok)
	}
	if _, ok := a.Authenticate("agent-secre"); ok {
		t.Fatal("prefix of a token authenticated")
	}
	if err := RequireWorkspace(got, "other-demo"); err == nil {
		t.Fatal("cross-workspace id was accepted")
	}
	if err := RequireWorkspace(got, "banking-demo"); err != nil {
		t.Fatal(err)
	}
}

func TestMiddleware(t *testing.T) {
	a, err := New([]config.Token{{Token: "agent-secret", WorkspaceID: "banking-demo", Role: RoleCurator}})
	if err != nil {
		t.Fatal(err)
	}
	handler := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := FromContext(r.Context())
		if !ok || p.Role != RoleCurator {
			http.Error(w, "missing principal", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/workspace", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/workspace", nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
}
