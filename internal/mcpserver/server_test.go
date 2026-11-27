package mcpserver_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolsAndResources(t *testing.T) {
	ctx := context.Background()
	session := connect(t, "")
	defer session.Close()
	if got := session.InitializeResult().ProtocolVersion; got != "2026-07-28" {
		t.Fatalf("protocol = %s", got)
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
		if strings.Contains(tool.Name, "git") || strings.Contains(tool.Name, "merge") || strings.Contains(tool.Name, "source") {
			t.Fatalf("privileged tool exposed: %s", tool.Name)
		}
	}
	for _, name := range []string{"knowledge.search", "knowledge.get", "knowledge.report_gap", "knowledge.submit_feedback", "knowledge.get_gap_status"} {
		if !names[name] {
			t.Fatalf("missing %s in %v", name, names)
		}
	}
	if len(names) != 5 {
		t.Fatalf("tool count = %d", len(names))
	}

	search, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "knowledge.search",
		Arguments: map[string]any{"query": "dedupe"},
	})
	if err != nil || search.IsError {
		t.Fatalf("search err=%v result=%+v", err, search)
	}
	text := contentText(t, search)
	if !strings.Contains(text, "kb://banking/events/movement-lifecycle") {
		t.Fatalf("search text = %s", text)
	}

	got, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "knowledge.get",
		Arguments: map[string]any{"uri": "kb://banking/events/movement-lifecycle"},
	})
	if err != nil || got.IsError {
		t.Fatalf("get err=%v %+v", err, got)
	}
	if !strings.Contains(contentText(t, got), "movementId") {
		t.Fatal("approved body missing")
	}

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "kb://banking/events/movement-lifecycle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Contents) != 1 || !strings.Contains(res.Contents[0].Text, "movementId") {
		t.Fatalf("resource = %+v", res.Contents)
	}

	cross, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "knowledge.search",
		Arguments: map[string]any{"query": "dedupe", "workspace_id": "other-demo"},
	})
	if err == nil && (cross == nil || !cross.IsError) {
		t.Fatal("cross-workspace search was accepted")
	}

	reported, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "knowledge.report_gap",
		Arguments: map[string]any{
			"workspace_id":    "banking-demo",
			"agent_run_id":    "run-1",
			"task_kind":       "integration-testing",
			"category":        "stale-instruction",
			"summary":         "dedupe guide is wrong",
			"observed":        "contract uses eventId",
			"expected":        "guide names another key",
			"client_event_id": "evt-1",
			"knowledge_refs":  []string{"kb://banking/events/movement-lifecycle"},
		},
	})
	if err != nil || reported.IsError {
		t.Fatalf("report err=%v text=%s", err, contentText(t, reported))
	}
	body := contentText(t, reported)
	if !strings.Contains(body, "new") {
		t.Fatalf("report body = %s", body)
	}
}

func TestOlderProtocolStillConnects(t *testing.T) {
	ctx := context.Background()
	session := connect(t, "2025-06-18")
	defer session.Close()
	if got := session.InitializeResult().ProtocolVersion; got != "2025-06-18" {
		t.Fatalf("negotiated %s", got)
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "knowledge.search",
		Arguments: map[string]any{"query": "dedupe"},
	}); err != nil {
		t.Fatal(err)
	}
}

type fakeReader struct{}

func (fakeReader) SearchRevision(_ context.Context, workspaceID, query, _ string, _ int, _ string) ([]index.Hit, error) {
	if workspaceID != "banking-demo" {
		tWorkspace(workspaceID)
	}
	if !strings.Contains(strings.ToLower(query), "dedupe") && !strings.Contains(query, "DRAFT") {
		return nil, nil
	}
	if strings.Contains(query, "DRAFT") {
		return nil, nil
	}
	return []index.Hit{{
		URI:      "kb://banking/events/movement-lifecycle",
		Title:    "Movement lifecycle events",
		Revision: "fixture-sha",
		Snippet:  "deduplicate by movementId",
		Stale:    true,
	}}, nil
}

func (fakeReader) Get(_ context.Context, workspaceID, uri, _ string) (index.Item, error) {
	if workspaceID != "banking-demo" || uri != "kb://banking/events/movement-lifecycle" {
		return index.Item{}, index.ErrNotFound
	}
	return index.Item{
		Hit:  index.Hit{URI: uri, Revision: "fixture-sha", Title: "Movement lifecycle events"},
		Body: "External consumers must deduplicate by `movementId`.",
	}, nil
}

func tWorkspace(id string) {
	panic("search left workspace banking-demo for " + id)
}

func connect(t *testing.T, protocol string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	server := mcpserver.New(mcpserver.Deps{
		WorkspaceID: "banking-demo",
		AgentID:     "qe",
		Reader:      fakeReader{},
		Gaps:        gaps.NewMemory(),
	})
	left, right := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, left, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	var opts *mcp.ClientSessionOptions
	if protocol != "" {
		opts = &mcp.ClientSessionOptions{ProtocolVersion: protocol}
	}
	session, err := client.Connect(ctx, right, opts)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func contentText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	if res.StructuredContent != nil {
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
	}
	return b.String()
}
