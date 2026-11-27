package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/auth"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/traces"
)

func (s *Server) reportGap(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.principal(w, r)
	if !ok {
		return
	}
	if principal.Role != auth.RoleAgent && principal.Role != auth.RoleCurator {
		writeError(w, http.StatusForbidden, "gap intake requires an agent or curator credential")
		return
	}
	if s.GapIntake == nil {
		writeError(w, http.StatusServiceUnavailable, "gap intake is not configured")
		return
	}
	var report domain.GapReport
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil || decoder.Decode(new(any)) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid gap report")
		return
	}
	if err := auth.RequireWorkspace(principal, report.WorkspaceID); err != nil {
		writeError(w, http.StatusForbidden, "workspace does not match credential")
		return
	}
	report.WorkspaceID = principal.WorkspaceID
	if err := domain.ValidateReport(report); err != nil {
		if errors.Is(err, domain.ErrInvalidTraceRef) {
			writeError(w, http.StatusBadRequest, err.Error())
		} else {
			writeError(w, http.StatusBadRequest, "invalid gap report")
		}
		return
	}
	gap, err := s.GapIntake.ReportAs(r.Context(), report, "http", principal)
	switch {
	case errors.Is(err, traces.ErrNotFound):
		writeError(w, http.StatusNotFound, "trace not found")
		return
	case errors.Is(err, gaps.ErrSecret):
		writeError(w, http.StatusBadRequest, "gap report contained a secret and was rejected")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "gap could not be recorded")
		return
	}
	if s.StartGap != nil && principal.WorkspaceID == s.ReviewWorkspace {
		target := gap.ID
		if gap.CanonicalID != "" {
			target = gap.CanonicalID
		}
		if err := s.StartGap(r.Context(), target); err != nil {
			writeError(w, http.StatusServiceUnavailable, "gap recorded but investigation could not be queued")
			return
		}
	}
	writeJSON(w, http.StatusAccepted, gap)
}
