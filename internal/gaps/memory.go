// Package gaps stores gap reports. The memory implementation is for MCP tests
// and for a process that has not opened Postgres. It does not de-duplicate.
package gaps

import (
	"context"
	"fmt"
	"sync"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/auth"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/traces"
	"github.com/google/uuid"
)

// ErrNotFound means the gap is not in this workspace.
var ErrNotFound = fmt.Errorf("gap not found")

// Memory is a process-local gap store.
type Memory struct {
	mu       sync.Mutex
	gaps     map[string]domain.Gap
	feedback map[string]domain.Feedback
}

// NewMemory returns an empty store.
func NewMemory() *Memory {
	return &Memory{gaps: map[string]domain.Gap{}, feedback: map[string]domain.Feedback{}}
}

// Report validates and stores a gap in state "new".
func (m *Memory) Report(ctx context.Context, report domain.GapReport, agentID string) (domain.Gap, error) {
	return m.ReportAs(ctx, report, agentID, auth.Principal{})
}

func (m *Memory) ReportAs(_ context.Context, report domain.GapReport, _ string, _ auth.Principal) (domain.Gap, error) {
	if err := domain.ValidateReport(report); err != nil {
		return domain.Gap{}, err
	}
	if report.TraceRef != nil {
		return domain.Gap{}, traces.ErrNotFound
	}
	gap := domain.Gap{
		ID:               uuid.NewString(),
		State:            "new",
		ReportedSeverity: report.Severity,
		Report:           report,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gaps[gap.ID] = gap
	return gap, nil
}

// Status returns a gap only when it belongs to workspaceID.
func (m *Memory) Status(_ context.Context, workspaceID, id string) (domain.Gap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	gap, ok := m.gaps[id]
	if !ok || gap.Report.WorkspaceID != workspaceID {
		return domain.Gap{}, ErrNotFound
	}
	return gap, nil
}

// Feedback stores an observation and returns its id.
func (m *Memory) Feedback(_ context.Context, fb domain.Feedback) (string, error) {
	if err := domain.ValidateWorkspaceID(fb.WorkspaceID); err != nil {
		return "", err
	}
	if fb.KnowledgeURI == "" || fb.Revision == "" || fb.Observation == "" {
		return "", fmt.Errorf("feedback is missing a required field")
	}
	id := uuid.NewString()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.feedback[id] = fb
	return id, nil
}
