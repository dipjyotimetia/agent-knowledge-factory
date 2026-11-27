// Package mcpserver exposes approved knowledge and gap reporting over MCP.
// The tool list is fixed. It does not include Git mutation or source-host search.
package mcpserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/auth"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/buildinfo"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Reader is the published knowledge index.
type Reader interface {
	SearchRevision(ctx context.Context, workspaceID, query, itemType string, limit int, revision string) ([]index.Hit, error)
	Get(ctx context.Context, workspaceID, uri, revision string) (index.Item, error)
}

// Reporter accepts gap reports for one workspace.
type Reporter interface {
	ReportAs(ctx context.Context, report domain.GapReport, agentID string, principal auth.Principal) (domain.Gap, error)
	Status(ctx context.Context, workspaceID, id string) (domain.Gap, error)
	Feedback(ctx context.Context, fb domain.Feedback) (string, error)
}

// Deps is the process boundary. WorkspaceID comes from configuration, not from
// the model, except as a value that must match.
type Deps struct {
	WorkspaceID string
	AgentID     string
	ActorID     string
	Reader      Reader
	Gaps        Reporter
	// Start begins curation for the canonical gap. Nil skips orchestration.
	Start func(ctx context.Context, gapID string) error
}

// New builds an MCP server. The workspace is fixed for the process.
func New(deps Deps) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "agent-knowledge-factory",
		Version: buildinfo.Version,
	}, nil)
	h := &handlers{deps: deps}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "knowledge.search",
		Description: "Search approved knowledge in this workspace. Results include revision and provenance.",
	}, h.search)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "knowledge.get",
		Description: "Read one approved knowledge item by URI.",
	}, h.get)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "knowledge.report_gap",
		Description: "Report a missing, stale, or contradictory knowledge item. This does not change knowledge.",
	}, h.report)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "knowledge.submit_feedback",
		Description: "Attach an observation to a knowledge revision.",
	}, h.feedback)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "knowledge.get_gap_status",
		Description: "Read the lifecycle state of a gap in this workspace.",
	}, h.status)
	server.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "kb://{authority}/{+path}",
		Name:        "knowledge",
		Description: "Approved knowledge item",
		MIMEType:    "text/markdown",
	}, h.read)
	return server
}

type handlers struct {
	deps Deps
}

type searchIn struct {
	Query       string `json:"query" jsonschema:"text to search"`
	Revision    string `json:"revision,omitempty" jsonschema:"must match the active revision when set"`
	Type        string `json:"type,omitempty" jsonschema:"concept or skill"`
	Limit       int    `json:"limit,omitempty" jsonschema:"maximum results"`
	WorkspaceID string `json:"workspace_id,omitempty" jsonschema:"must match the configured workspace when set"`
}

type searchOut struct {
	Results []index.Hit `json:"results"`
}

func (h *handlers) search(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
	if err := h.sameWorkspace(in.WorkspaceID); err != nil {
		return nil, searchOut{}, err
	}
	if h.deps.Reader == nil {
		return nil, searchOut{}, errors.New("search is not configured")
	}
	hits, err := h.deps.Reader.SearchRevision(ctx, h.deps.WorkspaceID, in.Query, in.Type, in.Limit, in.Revision)
	if err != nil {
		return nil, searchOut{}, err
	}
	return nil, searchOut{Results: hits}, nil
}

type getIn struct {
	URI         string `json:"uri" jsonschema:"canonical knowledge URI"`
	Revision    string `json:"revision,omitempty" jsonschema:"must match the active revision when set"`
	WorkspaceID string `json:"workspace_id,omitempty"`
}

func (h *handlers) get(ctx context.Context, _ *mcp.CallToolRequest, in getIn) (*mcp.CallToolResult, index.Item, error) {
	if err := h.sameWorkspace(in.WorkspaceID); err != nil {
		return nil, index.Item{}, err
	}
	if h.deps.Reader == nil {
		return nil, index.Item{}, errors.New("knowledge is not configured")
	}
	item, err := h.deps.Reader.Get(ctx, h.deps.WorkspaceID, in.URI, in.Revision)
	if errors.Is(err, index.ErrNotFound) {
		return nil, index.Item{}, mcp.ResourceNotFoundError(in.URI)
	}
	return nil, item, err
}

func (h *handlers) report(ctx context.Context, _ *mcp.CallToolRequest, in domain.GapReport) (*mcp.CallToolResult, domain.Gap, error) {
	if in.WorkspaceID == "" {
		in.WorkspaceID = h.deps.WorkspaceID
	}
	if err := h.sameWorkspace(in.WorkspaceID); err != nil {
		return nil, domain.Gap{}, err
	}
	gap, err := h.deps.Gaps.ReportAs(ctx, in, h.agent(), auth.Principal{WorkspaceID: h.deps.WorkspaceID, Role: auth.RoleAgent, ActorID: h.deps.ActorID})
	if err != nil {
		return nil, domain.Gap{}, err
	}
	target := gap.ID
	if gap.CanonicalID != "" {
		target = gap.CanonicalID
	}
	if h.deps.Start != nil {
		if err := h.deps.Start(ctx, target); err != nil {
			return nil, gap, err
		}
	}
	return nil, gap, nil
}

type feedbackIn struct {
	KnowledgeURI string `json:"knowledge_uri"`
	Revision     string `json:"revision"`
	Observation  string `json:"observation"`
	AgentRunID   string `json:"agent_run_id,omitempty"`
	WorkspaceID  string `json:"workspace_id,omitempty"`
}

type feedbackOut struct {
	ID string `json:"id"`
}

func (h *handlers) feedback(ctx context.Context, _ *mcp.CallToolRequest, in feedbackIn) (*mcp.CallToolResult, feedbackOut, error) {
	if in.WorkspaceID == "" {
		in.WorkspaceID = h.deps.WorkspaceID
	}
	if err := h.sameWorkspace(in.WorkspaceID); err != nil {
		return nil, feedbackOut{}, err
	}
	id, err := h.deps.Gaps.Feedback(ctx, domain.Feedback{
		WorkspaceID:  h.deps.WorkspaceID,
		KnowledgeURI: in.KnowledgeURI,
		Revision:     in.Revision,
		Observation:  in.Observation,
		AgentRunID:   in.AgentRunID,
	})
	return nil, feedbackOut{ID: id}, err
}

type statusIn struct {
	GapID       string `json:"gap_id"`
	WorkspaceID string `json:"workspace_id,omitempty"`
}

func (h *handlers) status(ctx context.Context, _ *mcp.CallToolRequest, in statusIn) (*mcp.CallToolResult, domain.Gap, error) {
	if err := h.sameWorkspace(in.WorkspaceID); err != nil {
		return nil, domain.Gap{}, err
	}
	gap, err := h.deps.Gaps.Status(ctx, h.deps.WorkspaceID, in.GapID)
	if errors.Is(err, gaps.ErrNotFound) {
		return nil, domain.Gap{}, fmt.Errorf("gap not found")
	}
	gap.Report.TraceRef = nil
	return nil, gap, err
}

func (h *handlers) read(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	if h.deps.Reader == nil {
		return nil, mcp.ResourceNotFoundError(req.Params.URI)
	}
	item, err := h.deps.Reader.Get(ctx, h.deps.WorkspaceID, req.Params.URI, "")
	if errors.Is(err, index.ErrNotFound) {
		return nil, mcp.ResourceNotFoundError(req.Params.URI)
	}
	if err != nil {
		return nil, err
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
		URI:      req.Params.URI,
		MIMEType: "text/markdown",
		Text:     item.Body,
	}}}, nil
}

func (h *handlers) sameWorkspace(supplied string) error {
	if supplied == "" || supplied == h.deps.WorkspaceID {
		return nil
	}
	return fmt.Errorf("workspace does not match this server")
}

func (h *handlers) agent() string {
	if h.deps.AgentID == "" {
		return "mcp"
	}
	return h.deps.AgentID
}
