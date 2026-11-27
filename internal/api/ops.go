package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/auth"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publication"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
	"github.com/jackc/pgx/v5"
)

// GapReader lists and loads gaps inside the caller's workspace.
type GapReader interface {
	List(ctx context.Context, workspaceID string) ([]domain.Gap, error)
	Status(ctx context.Context, workspaceID, id string) (domain.Gap, error)
}

// Querier runs a workspace-scoped transaction.
type Querier interface {
	WithWorkspace(ctx context.Context, workspaceID string, fn func(context.Context, pgx.Tx) error) error
}

func (s *Server) listGaps(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if s.Gaps == nil {
		writeError(w, http.StatusServiceUnavailable, "gap store is not configured")
		return
	}
	items, err := s.Gaps.List(r.Context(), principal.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gaps": items})
}

func (s *Server) showGap(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if s.Gaps == nil {
		writeError(w, http.StatusServiceUnavailable, "gap store is not configured")
		return
	}
	gap, err := s.Gaps.Status(r.Context(), principal.WorkspaceID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "gap not found")
		return
	}
	writeJSON(w, http.StatusOK, gap)
}

func (s *Server) decideGap(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if principal.Role != auth.RoleReviewer && principal.Role != auth.RoleCurator {
		writeError(w, http.StatusForbidden, "reviewer or curator role required")
		return
	}
	if s.Reviews == nil && s.Publication == nil {
		writeError(w, http.StatusServiceUnavailable, "review signaler is not configured")
		return
	}
	var body struct {
		Action   string `json:"action"`
		MergeSHA string `json:"merge_sha"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "json rejected")
		return
	}
	signal := workflows.ReviewSignal{Reason: body.Reason}
	switch body.Action {
	case "reject":
		signal.Rejected = true
	case "merged":
		signal.Merged = true
		signal.MergeSHA = body.MergeSHA
	default:
		writeError(w, http.StatusBadRequest, "action must be reject or merged")
		return
	}
	scope := s.ReviewWorkspace
	if s.Publication != nil {
		scope = s.Publication.Workspace
	}
	if scope != "" && principal.WorkspaceID != scope {
		writeError(w, http.StatusForbidden, "workspace rejected")
		return
	}
	if s.Publication != nil && signal.Merged {
		id, err := s.Publication.RequestRelease(r.Context(), publication.Request{Trigger: "pr-merge", GapID: r.PathValue("id")})
		if err != nil {
			writeError(w, http.StatusBadGateway, "release enqueue failed")
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending", "operation_id": id})
		return
	}
	if s.Reviews == nil {
		writeError(w, http.StatusServiceUnavailable, "review signaler unavailable")
		return
	}
	if err := s.Reviews.Signal(r.Context(), r.PathValue("id"), signal); err != nil {
		writeError(w, http.StatusBadGateway, "signal failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "signaled"})
}

func (s *Server) listProposals(w http.ResponseWriter, r *http.Request) {
	s.queryJSON(w, r, `
		SELECT id::text, decision, approval_status, coalesce(git_pr_url, '')
		FROM proposals ORDER BY created_at DESC LIMIT 50
	`)
}

func (s *Server) listEvaluations(w http.ResponseWriter, r *http.Request) {
	s.queryJSON(w, r, `
		SELECT id::text, phase, passed, failed, critical_regressions
		FROM evaluation_runs ORDER BY created_at DESC LIMIT 50
	`)
}

func (s *Server) queryJSON(w http.ResponseWriter, r *http.Request, sql string) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if s.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "database is not configured")
		return
	}
	var rowsOut []map[string]any
	err := s.DB.WithWorkspace(r.Context(), principal.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			values, err := rows.Values()
			if err != nil {
				return err
			}
			descs := rows.FieldDescriptions()
			item := map[string]any{}
			for i, desc := range descs {
				item[string(desc.Name)] = values[i]
			}
			rowsOut = append(rowsOut, item)
		}
		return rows.Err()
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if rowsOut == nil {
		rowsOut = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": rowsOut})
}
