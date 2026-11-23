package api

import (
	"context"
	"net/http"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/auth"
	"github.com/jackc/pgx/v5"
)

// review is a read-only view of gaps, proposals, evaluations, and evidence.
// It does not merge. GitHub remains the merge authority.
func (s *Server) review(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if s.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "database is not configured")
		return
	}
	view := map[string]any{"workspace_id": principal.WorkspaceID}
	queries := []struct {
		key string
		sql string
	}{
		{"gaps", `SELECT id::text, state, summary FROM gap_reports ORDER BY created_at DESC LIMIT 50`},
		{"proposals", `SELECT id::text, decision, approval_status, coalesce(git_pr_url, '') FROM proposals ORDER BY created_at DESC LIMIT 50`},
		{"evaluations", `SELECT id::text, phase, passed::text, failed::text, critical_regressions::text FROM evaluation_runs ORDER BY created_at DESC LIMIT 50`},
		{"evidence", `SELECT id::text, classification, verification_state, location FROM evidence_links ORDER BY created_at DESC LIMIT 50`},
	}
	for _, query := range queries {
		rowsOut, err := s.reviewRows(r, principal.WorkspaceID, query.sql)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		view[query.key] = rowsOut
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) reviewRows(r *http.Request, workspaceID, sql string) ([]map[string]any, error) {
	var rowsOut []map[string]any
	err := s.DB.WithWorkspace(r.Context(), workspaceID, func(ctx context.Context, tx pgx.Tx) error {
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
	if rowsOut == nil {
		rowsOut = []map[string]any{}
	}
	return rowsOut, err
}
