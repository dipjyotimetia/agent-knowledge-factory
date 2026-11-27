package gaps_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/auth"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/testutil"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/traces"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func Test24TraceScopeExpiryAndRetention(t *testing.T) {
	admin, app := testutil.OpenMigrated(t)
	ctx := context.Background()
	ws, other := "trace-"+uuid.NewString(), "trace-"+uuid.NewString()
	for _, workspace := range []string{ws, other} {
		if err := admin.CreateWorkspace(ctx, postgres.Workspace{ID: workspace, DisplayName: workspace, RetentionDays: 1}); err != nil {
			t.Fatal(err)
		}
	}
	store, svc := traces.Store{DB: app}, gaps.NewService(app)
	operator := auth.Principal{WorkspaceID: ws, Role: auth.RoleCurator, ActorID: "operator"}
	agent := auth.Principal{WorkspaceID: ws, Role: auth.RoleAgent, ActorID: "owner"}
	const humanText = "Please include a report header."
	raw := []byte(`{"turns":[{"role":"user","text":"` + humanText + `","email":"pii-sentinel@example.test"},
		{"role":"assistant","text":"Authorization: Bearer opaque-credential-sentinel"}]}`)
	ref, err := store.Import(ctx, operator, "run", agent.ActorID, raw)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := domain.TraceID(ref)
	if !ok {
		t.Fatal("import must return an opaque ID")
	}
	report := domain.GapReport{WorkspaceID: ws, AgentRunID: "run", TaskKind: "test", Category: domain.CategoryMissingProcedure,
		Summary: "A procedure is missing", Observed: "no steps", Expected: "specific steps", ClientEventID: "event", TraceRef: &ref}
	for _, bad := range []string{"../../file", "/private/tmp/file", "file:///safe.json", "http://local",
		"trace:%2fetc%2fpasswd", "trace:" + strings.ToUpper(id), ref + "\x00", ""} {
		input := report
		input.TraceRef = &bad
		if _, err := svc.ReportAs(ctx, input, "fixture", agent); !errors.Is(err, domain.ErrInvalidTraceRef) {
			t.Errorf("invalid reference returned %v", err)
		}
	}
	for _, input := range []struct {
		workspace, runID, actor, role, ref string
	}{
		{ws, "different-run", agent.ActorID, auth.RoleAgent, ref},
		{ws, "run", "different-actor", auth.RoleAgent, ref},
		{ws, "run", "", auth.RoleCurator, ref},
		{other, "run", agent.ActorID, auth.RoleAgent, ref},
		{ws, "run", agent.ActorID, auth.RoleReviewer, ref},
		{ws, "run", agent.ActorID, auth.RoleAgent, "trace:" + uuid.NewString()},
	} {
		reportCopy := report
		reportCopy.WorkspaceID, reportCopy.AgentRunID, reportCopy.TraceRef = input.workspace, input.runID, &input.ref
		who := auth.Principal{WorkspaceID: input.workspace, ActorID: input.actor, Role: input.role}
		if _, err := svc.ReportAs(ctx, reportCopy, "fixture", who); !errors.Is(err, traces.ErrNotFound) {
			t.Errorf("trace scope returned %v", err)
		}
	}
	gap, err := svc.ReportAs(ctx, report, "fixture", agent)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := svc.ReportAs(ctx, report, "fixture", agent); err != nil || got.ID != gap.ID {
		t.Fatalf("owner retry: %v", err)
	}
	if _, err := svc.ReportAs(ctx, report, "fixture", operator); err != nil {
		t.Fatalf("trusted curator link: %v", err)
	}
	attempt := uuid.NewString()
	if err := app.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO investigation_attempts(workspace_id,id,gap_id,policy_version,state)
			VALUES ($1,$2,$3,'test','running')`, ws, attempt, gap.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	doc, err := store.GetForInvestigation(ctx, ws, attempt)
	if asks := traces.HumanAsks(doc); err != nil || len(asks) != 1 || asks[0] != humanText {
		t.Fatalf("unexpected trace access: %v", err)
	}
	if err := admin.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		var payload string
		if err := tx.QueryRow(ctx, `SELECT sanitised_payload::text FROM trace_records WHERE id=$1`, id).Scan(&payload); err != nil {
			return err
		}
		if strings.Contains(payload, "sentinel") {
			t.Fatal("raw sentinel reached the database")
		}
		_, err := tx.Exec(ctx, `UPDATE trace_records SET created_at=now()-interval '2 days',expires_at=now()-interval '1 day' WHERE id=$1`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportAs(ctx, report, "fixture", agent); !errors.Is(err, traces.ErrNotFound) {
		t.Fatalf("expired idempotent trace: %v", err)
	}
	if doc, err := store.GetForInvestigation(ctx, ws, attempt); err != nil || len(doc.Turns) != 0 {
		t.Fatalf("expired trace was visible: %v", err)
	}
	if err := app.EnforceRetention(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := app.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		var outcome string
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM trace_records WHERE id=$1),trace_outcome
			FROM investigation_attempts WHERE id=$2`, id, attempt).Scan(&exists, &outcome)
		if err == nil && (exists || outcome != "unavailable") {
			t.Fatalf("expired payload exists=%t, trace outcome=%q", exists, outcome)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportAs(ctx, report, "fixture", agent); !errors.Is(err, traces.ErrNotFound) {
		t.Fatalf("deleted trace: %v", err)
	}
	report.TraceRef = nil
	if _, err := svc.Report(ctx, report, "legacy"); err != nil {
		t.Fatalf("trace-free legacy caller: %v", err)
	}
}
