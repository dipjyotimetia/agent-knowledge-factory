package demo_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/api"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/auth"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/evaluations"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/mcpserver"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publication"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/testutil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func Test24HTTPAndMCPUseTheActiveRelease(t *testing.T) {
	ctx := context.Background()
	admin, app := testutil.OpenMigrated(t)
	workspace, other := "revision-"+uuid.NewString(), "revision-"+uuid.NewString()
	active, legacy, staged := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	const uri = "kb://scope/note"
	const private = "private-staged-fixture-marker"
	const authoritative = "github://fixture/source/policy.md"
	const manifest = "---\nokf_version: \"0.3\"\nx-akf-uri-prefix: kb://scope\n---\n\n# Scope\n"
	const note = "---\ntype: Note\ntitle: Pipeline protocol\nstatus: stable\nsources:\n  - resource: " + authoritative + "\n---\n\n"
	files := map[string]string{"index.md": manifest, "note.md": note + "The pipeline protocol is approved.\n"}
	forge := publish.NewMemory(active, files)
	identity, _, err := forge.Resolve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{workspace, other} {
		if err := admin.CreateWorkspace(ctx, postgres.Workspace{ID: ws, DisplayName: ws, KnowledgeRepo: identity.Repository}); err != nil {
			t.Fatal(err)
		}
	}
	idx, fixture := index.New(app), index.NewFixture(app)
	authn, err := auth.New([]config.Token{
		{Token: "scoped", WorkspaceID: workspace, Role: auth.RoleAgent},
		{Token: "other", WorkspaceID: other, Role: auth.RoleAgent},
		{Token: "review", WorkspaceID: workspace, Role: auth.RoleReviewer},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(nil, app, app, authn)
	srv.SetKnowledge(idx.Published())
	srv.DB = app
	handler := srv.Handler()
	get := func(route, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	knowledge := "/v1/knowledge?uri=" + url.QueryEscape(uri)
	root := t.TempDir()
	for name, body := range map[string]string{"index.md": manifest, "note.md": note + private, "hidden.md": note + private} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.Reindex(ctx, workspace, legacy, root); err != nil {
		t.Fatal(err)
	}
	if got := get(knowledge, "scoped"); got.Code != http.StatusNotFound {
		t.Fatalf("legacy pointer before bootstrap: %d", got.Code)
	}
	caseFile := filepath.Join(t.TempDir(), "suite.json")
	suite := make([]evaluations.Case, 20)
	for i := range suite {
		suite[i] = evaluations.Case{ID: strconv.Itoa(i), Kind: "documentation-gap", Critical: true,
			RetrievalQuery: "pipeline protocol", Answer: "approved", Extract: "contains"}
	}
	raw, err := json.Marshal(suite)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caseFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	releases := publication.Service{DB: app, Index: idx, Forge: forge, Workspace: workspace, Identity: identity,
		CasesPath: caseFile, PolicyDigest: "fixture-policy", Sources: func(context.Context) (publication.EvidenceSet, error) {
			return publication.EvidenceSet{{ID: "policy", Location: authoritative, Classification: "policy",
				Revision: strings.Repeat("d", 40), Digest: "fixture", Content: "The pipeline protocol is approved."}}, nil
		}}
	if err := app.WithWorkspace(ctx, workspace, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE workspaces SET publication_hold=false WHERE id=$1`, workspace)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := releases.RequestRelease(ctx, publication.Request{Trigger: "bootstrap"}); err != nil {
		t.Fatal(err)
	}
	if worked, err := releases.Process(ctx); err != nil || !worked {
		t.Fatalf("bootstrap ran=%t err=%v", worked, err)
	}
	if err := fixture.Stage(ctx, workspace, staged, root); err != nil {
		t.Fatal(err)
	}
	opID, err := releases.RequestRelease(ctx, publication.Request{Trigger: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	candidate := uuid.NewString()
	if err := app.WithWorkspace(ctx, workspace, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO publication_candidates(workspace_id,id,operation_id,tree_digest) VALUES ($1,$2,$3,'fixture')`,
			workspace, candidate, opID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []struct{ workspace, operation string }{{other, opID}, {workspace, ""}, {workspace, uuid.NewString()}} {
		if _, err := idx.StageCandidate(ctx, scope.workspace, candidate, scope.operation, root); !errors.Is(err, index.ErrNotFound) {
			t.Fatalf("candidate capability escaped scope: %v", err)
		}
	}
	reader, err := idx.StageCandidate(ctx, workspace, candidate, opID, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.StageCandidate(ctx, workspace, candidate, opID, root); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("already staged candidate was writable: %v", err)
	}
	if item, err := reader.Get(ctx, "kb://scope/hidden"); err != nil || !strings.Contains(item.Body, private) {
		t.Fatalf("scoped evaluator could not read its candidate: %v", err)
	}
	if hits, err := reader.Search(ctx, "pipeline protocol", 5); err != nil || len(hits) != 2 {
		t.Fatalf("scoped evaluator results=%d: %v", len(hits), err)
	}

	search := "/v1/search?q=pipeline+protocol"
	for _, endpoint := range []string{knowledge, search} {
		for _, revision := range []string{legacy, staged, "candidate:" + candidate, strings.Repeat("f", 40)} {
			got := get(endpoint+"&revision="+url.QueryEscape(revision), "scoped")
			if got.Code != http.StatusNotFound || strings.Contains(got.Body.String(), private) {
				t.Errorf("inactive HTTP revision status=%d", got.Code)
			}
		}
		if got := get(endpoint+"&revision="+active, "scoped"); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), active) {
			t.Errorf("active HTTP revision status=%d", got.Code)
		}
	}
	if got := get(knowledge, "other"); got.Code != http.StatusNotFound {
		t.Errorf("cross-workspace HTTP status=%d", got.Code)
	}
	if err := app.WithWorkspace(ctx, workspace, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO evaluation_runs(id,workspace_id,phase,knowledge_sha,passed,failed,critical_regressions,results)
			VALUES ($1,$2,'after','proposal',0,1,0,$3)`, uuid.NewString(), workspace, []map[string]string{{"detail": private}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"/v1/review", "/v1/evaluations"} {
		got := get(endpoint, "review")
		if got.Code != http.StatusOK || strings.Contains(got.Body.String(), private) ||
			strings.Contains(got.Body.String(), candidate) || strings.Contains(got.Body.String(), `"detail"`) {
			t.Errorf("private evaluation data in %s (status %d)", endpoint, got.Code)
		}
	}

	server := mcpserver.New(mcpserver.Deps{WorkspaceID: workspace, ActorID: "owner", Reader: idx.Published(), Gaps: gaps.NewService(app)})
	left, right := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, left, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "fixture"}, nil).Connect(ctx, right, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, revision := range []string{legacy, staged, "candidate:" + candidate} {
		for name, args := range map[string]map[string]any{
			"knowledge.get":    {"uri": uri, "revision": revision},
			"knowledge.search": {"query": "pipeline protocol", "revision": revision},
		} {
			got, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
			if err == nil && (got == nil || !got.IsError) {
				t.Errorf("MCP %s read an inactive revision", name)
			}
		}
	}
	for _, resource := range []string{"kb://scope/hidden", uri + "?revision=" + staged} {
		if _, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: resource}); err == nil {
			t.Error("MCP resource read a private document")
		}
	}
	if got, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri}); err != nil ||
		len(got.Contents) != 1 || !strings.Contains(got.Contents[0].Text, "approved") {
		t.Fatalf("active MCP resource unavailable: %v", err)
	}

	link := filepath.Join(root, "trace-link")
	if err := os.Symlink(filepath.Join(root, "hidden.md"), link); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{link, "../../hidden.md", "file:///private/trace.json", "https://example.test/trace", "trace:%2e%2e"} {
		got, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "knowledge.report_gap", Arguments: map[string]any{
			"workspace_id": workspace, "agent_run_id": "run", "task_kind": "test", "category": "missing-procedure",
			"summary": "missing", "observed": "a", "expected": "b", "client_event_id": "path", "trace_ref": ref,
		}})
		if err == nil && (got == nil || !got.IsError) {
			t.Error("MCP gap intake accepted a local or remote path")
		}
	}
}
