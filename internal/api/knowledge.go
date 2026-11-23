package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/auth"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
)

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.principal(w, r)
	if !ok {
		return
	}
	if s.knowledge == nil {
		writeError(w, http.StatusServiceUnavailable, "search is not configured")
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "limit must be an integer")
			return
		}
		limit = parsed
	}
	hits, err := s.knowledge.Search(r.Context(), principal.WorkspaceID, r.URL.Query().Get("q"), r.URL.Query().Get("type"), limit)
	if errors.Is(err, index.ErrInvalidQuery) {
		writeError(w, http.StatusBadRequest, "query is required")
		return
	}
	if err != nil {
		s.log.Error("search", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": hits})
}

func (s *Server) getKnowledge(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.principal(w, r)
	if !ok {
		return
	}
	if s.knowledge == nil {
		writeError(w, http.StatusServiceUnavailable, "search is not configured")
		return
	}
	item, err := s.knowledge.Get(r.Context(), principal.WorkspaceID, r.URL.Query().Get("uri"), r.URL.Query().Get("revision"))
	if errors.Is(err, index.ErrNotFound) {
		writeError(w, http.StatusNotFound, "knowledge item not found")
		return
	}
	if err != nil {
		s.log.Error("get knowledge", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) principal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return auth.Principal{}, false
	}
	if err := auth.RequireWorkspace(principal, r.URL.Query().Get("workspace_id")); err != nil {
		writeError(w, http.StatusForbidden, "workspace does not match credential")
		return auth.Principal{}, false
	}
	return principal, true
}
