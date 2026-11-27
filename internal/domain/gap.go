package domain

import (
	"errors"
	"fmt"
)

// Gap categories accepted from agents. The service may reclassify during triage.
const (
	CategoryMissingProcedure = "missing-procedure"
	CategoryStaleInstruction = "stale-instruction"
	CategoryBadDataAccess    = "bad-data-access"
	CategoryContradiction    = "contradictory-sources"
	CategoryRetrievalFailure = "retrieval-failure"
	CategoryExecutionFailure = "execution-failure"
)

// GapReport is untrusted client input. Severity and evidence refs are leads.
type GapReport struct {
	WorkspaceID   string         `json:"workspace_id"`
	AgentRunID    string         `json:"agent_run_id"`
	TaskKind      string         `json:"task_kind"`
	Category      string         `json:"category"`
	Summary       string         `json:"summary"`
	Observed      string         `json:"observed"`
	Expected      string         `json:"expected"`
	KnowledgeRefs []string       `json:"knowledge_refs,omitempty"`
	EvidenceRefs  []string       `json:"evidence_refs,omitempty"`
	TraceRef      *string        `json:"trace_ref,omitempty" jsonschema:"opaque trace ID from an authorized local import"`
	ClientEventID string         `json:"client_event_id"`
	Severity      string         `json:"severity,omitempty"`
	Suggested     *SuggestedEdit `json:"suggested_edit,omitempty"`
}

// SuggestedEdit is an optional structured change from the reporting agent.
// The curator may use it only when the new text is grounded in evidence.
type SuggestedEdit struct {
	Path string `json:"path"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

// Gap is the service record. ID and State are assigned by the factory.
type Gap struct {
	ID               string    `json:"id"`
	State            string    `json:"state"`
	CanonicalID      string    `json:"canonical_id,omitempty"`
	DedupeSignature  string    `json:"dedupe_signature,omitempty"`
	ReportedSeverity string    `json:"reported_severity,omitempty"`
	Report           GapReport `json:"report"`
}

// Feedback is an observation about one published revision.
type Feedback struct {
	WorkspaceID  string `json:"workspace_id"`
	KnowledgeURI string `json:"knowledge_uri"`
	Revision     string `json:"revision"`
	Observation  string `json:"observation"`
	AgentRunID   string `json:"agent_run_id,omitempty"`
}

var ErrInvalidTraceRef = errors.New("invalid trace_ref")

// ValidateReport checks shape only. Secret scanning is a separate gate.
func ValidateReport(report GapReport) error {
	if report.TraceRef != nil {
		if _, ok := TraceID(*report.TraceRef); !ok {
			return ErrInvalidTraceRef
		}
	}
	if err := ValidateWorkspaceID(report.WorkspaceID); err != nil {
		return err
	}
	if report.AgentRunID == "" || report.TaskKind == "" || report.Summary == "" || report.Observed == "" || report.Expected == "" || report.ClientEventID == "" {
		return errors.New("gap report is missing a required field")
	}
	switch report.Category {
	case CategoryMissingProcedure, CategoryStaleInstruction, CategoryBadDataAccess, CategoryContradiction, CategoryRetrievalFailure, CategoryExecutionFailure:
	default:
		return fmt.Errorf("unknown gap category %q", report.Category)
	}
	if len(report.Summary) > 500 || len(report.Observed) > 4000 || len(report.Expected) > 4000 || len(report.ClientEventID) > 200 {
		return errors.New("gap report field exceeds the size limit")
	}
	if len(report.KnowledgeRefs) > 20 || len(report.EvidenceRefs) > 20 {
		return errors.New("gap report has too many references")
	}
	return nil
}
