package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/api"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/auth"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/testutil"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/traces"
	"github.com/google/uuid"
)

func Test24HTTPTraceIntakeUsesBearerActor(t *testing.T) {
	admin, app := testutil.OpenMigrated(t)
	ctx := context.Background()
	ws := "trace-" + uuid.NewString()
	if err := admin.CreateWorkspace(ctx, postgres.Workspace{ID: ws, DisplayName: ws}); err != nil {
		t.Fatal(err)
	}
	az, err := auth.New([]config.Token{
		{Token: "owner", WorkspaceID: ws, Role: auth.RoleAgent, ActorID: "owner"},
		{Token: "other", WorkspaceID: ws, Role: auth.RoleAgent, ActorID: "other"},
		{Token: "legacy", WorkspaceID: ws, Role: auth.RoleAgent},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(nil, app, app, az)
	srv.GapIntake = gaps.NewService(app)
	handler := srv.Handler()
	ref, err := (traces.Store{DB: app}).Import(ctx, auth.Principal{WorkspaceID: ws, ActorID: "operator", Role: auth.RoleCurator},
		"run", "owner", []byte(`[{"gen_ai.human":"fixture"}]`))
	if err != nil {
		t.Fatal(err)
	}
	report := domain.GapReport{WorkspaceID: ws, AgentRunID: "run", TaskKind: "test", Category: domain.CategoryMissingProcedure,
		Summary: "Missing procedure", Observed: "no steps", Expected: "steps", ClientEventID: "report"}
	submit := func(actor, reference string) *httptest.ResponseRecorder {
		t.Helper()
		report.TraceRef = nil
		if reference != "" {
			report.TraceRef = &reference
		}
		raw, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/gaps", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+actor)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if got := submit("owner", "file:///trace.json"); got.Code != http.StatusBadRequest || strings.Contains(got.Body.String(), "file:") {
		t.Fatalf("malformed reference status %d", got.Code)
	}
	missing := submit("owner", "trace:"+uuid.NewString())
	for _, actor := range []string{"other", "legacy"} {
		got := submit(actor, ref)
		if got.Code != http.StatusNotFound || got.Code != missing.Code || got.Body.String() != missing.Body.String() {
			t.Errorf("unscoped lookup status %d", got.Code)
		}
	}
	if got := submit("owner", ref); got.Code != http.StatusAccepted {
		t.Fatalf("owner status %d", got.Code)
	}
	if got := submit("legacy", ""); got.Code != http.StatusAccepted {
		t.Fatalf("legacy trace-free status %d", got.Code)
	}
}
