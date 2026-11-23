// Package api is the internal HTTP control API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/auth"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/buildinfo"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
)

// ReadyCheck is the database ping used by /readyz.
type ReadyCheck interface {
	Ping(context.Context) error
}

// WorkspaceReader loads the caller's workspace. Implementations must scope the
// read to the authenticated workspace id.
type WorkspaceReader interface {
	GetWorkspace(ctx context.Context, id string) (postgres.Workspace, error)
}

// Knowledge is the published-search surface. It is optional so health tests
// can construct a server without a database.
type Knowledge interface {
	Search(ctx context.Context, workspaceID, query, itemType string, limit int) ([]index.Hit, error)
	Get(ctx context.Context, workspaceID, uri, revision string) (index.Item, error)
}

// Server is the control API. It does not hold an installer connection.
type Server struct {
	log           *slog.Logger
	ready         ReadyCheck
	spaces        WorkspaceReader
	authn         *auth.Authenticator
	knowledge     Knowledge
	WebhookSecret string
	Deliveries    DeliveryStore
	Reviews       ReviewSignaler
	Gaps          GapReader
	DB            Querier
}

// New constructs the HTTP API.
func New(log *slog.Logger, ready ReadyCheck, spaces WorkspaceReader, authn *auth.Authenticator) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{log: log, ready: ready, spaces: spaces, authn: authn}
}

// SetKnowledge attaches published search. Call it before Handler.
func (s *Server) SetKnowledge(k Knowledge) {
	s.knowledge = k
}

// Handler returns the public routes. Health stays unauthenticated so a
// process manager can probe it. Workspace reads require a bearer token.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("POST /v1/github/webhook", s.githubWebhook)
	protected := http.NewServeMux()
	protected.HandleFunc("GET /v1/workspace", s.getWorkspace)
	protected.HandleFunc("GET /v1/search", s.search)
	protected.HandleFunc("GET /v1/knowledge", s.getKnowledge)
	protected.HandleFunc("GET /v1/gaps", s.listGaps)
	protected.HandleFunc("GET /v1/gaps/{id}", s.showGap)
	protected.HandleFunc("POST /v1/gaps/{id}/approval", s.decideGap)
	protected.HandleFunc("GET /v1/proposals", s.listProposals)
	protected.HandleFunc("GET /v1/evaluations", s.listEvaluations)
	if s.authn != nil {
		mux.Handle("/v1/", s.authn.Middleware(protected))
	} else {
		mux.Handle("/v1/", protected)
	}
	return mux
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": buildinfo.Version,
	})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.ready == nil {
		writeError(w, http.StatusServiceUnavailable, "database is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.ready.Ping(ctx); err != nil {
		s.log.Error("readiness", "err", err)
		writeError(w, http.StatusServiceUnavailable, "database is not ready")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) getWorkspace(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := auth.RequireWorkspace(principal, r.URL.Query().Get("workspace_id")); err != nil {
		writeError(w, http.StatusForbidden, "workspace does not match credential")
		return
	}
	if s.spaces == nil {
		writeError(w, http.StatusServiceUnavailable, "workspace store is not configured")
		return
	}
	ws, err := s.spaces.GetWorkspace(r.Context(), principal.WorkspaceID)
	if errors.Is(err, postgres.ErrNotFound) {
		writeError(w, http.StatusNotFound, "workspace is not initialized")
		return
	}
	if err != nil {
		s.log.Error("get workspace", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace_id":   ws.ID,
		"display_name":   ws.DisplayName,
		"knowledge_repo": ws.KnowledgeRepo,
		"published_sha":  ws.PublishedSHA,
		"retention_days": ws.RetentionDays,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
