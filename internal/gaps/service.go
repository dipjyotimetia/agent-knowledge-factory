package gaps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/audit"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/policy"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrSecret means the report was rejected and only a redacted audit row was kept.
var ErrSecret = errors.New("gap report contained a secret and was rejected")

// ErrConflict means the state transition is not allowed.
var ErrConflict = errors.New("gap state transition is not allowed")

// Service is the durable gap store.
type Service struct {
	DB *postgres.DB
}

// NewService returns the Postgres-backed gap store.
func NewService(db *postgres.DB) *Service {
	return &Service{DB: db}
}

// Event is one append-only transition.
type Event struct {
	From   string `json:"from_state"`
	To     string `json:"to_state"`
	Actor  string `json:"actor"`
	Reason string `json:"reason"`
}

// Report stores a gap. The same agent and client_event_id returns the original
// row. A different agent with the same failure signature is linked as a duplicate.
func (s *Service) Report(ctx context.Context, report domain.GapReport, agentID string) (domain.Gap, error) {
	if err := domain.ValidateReport(report); err != nil {
		return domain.Gap{}, err
	}
	if agentID == "" {
		return domain.Gap{}, errors.New("agent id is required")
	}
	if findings := scanReport(report); len(findings) > 0 {
		if err := s.auditRejection(ctx, report.WorkspaceID, agentID, findings); err != nil {
			return domain.Gap{}, err
		}
		return domain.Gap{}, ErrSecret
	}
	return s.report(ctx, report, agentID, false)
}

func (s *Service) report(ctx context.Context, report domain.GapReport, agentID string, retried bool) (domain.Gap, error) {
	var gap domain.Gap
	err := s.DB.WithWorkspace(ctx, report.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		existing, err := findIdempotent(ctx, tx, report.WorkspaceID, agentID, report.ClientEventID)
		if err == nil {
			gap = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		sig := signature(report)
		canonical, err := findOpen(ctx, tx, sig)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		gap = domain.Gap{
			ID:               uuid.NewString(),
			State:            domain.StateNew,
			DedupeSignature:  sig,
			ReportedSeverity: report.Severity,
			Report:           report,
		}
		reason := "accepted"
		if canonical.ID != "" {
			gap.State = domain.StateDuplicate
			gap.CanonicalID = canonical.ID
			reason = "linked to existing investigation " + canonical.ID
		}
		if err := insertGap(ctx, tx, gap, agentID); err != nil {
			return err
		}
		return insertEvent(ctx, tx, report.WorkspaceID, gap.ID, "", gap.State, agentID, reason)
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && !retried {
			return s.report(ctx, report, agentID, true)
		}
		return domain.Gap{}, err
	}
	return gap, nil
}

// Status returns one gap in the workspace.
func (s *Service) Status(ctx context.Context, workspaceID, id string) (domain.Gap, error) {
	var gap domain.Gap
	err := s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		gap, err = loadGap(ctx, tx, id)
		return err
	})
	return gap, err
}

// List returns gaps in the workspace, newest first.
func (s *Service) List(ctx context.Context, workspaceID string) ([]domain.Gap, error) {
	var out []domain.Gap
	err := s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, state, coalesce(canonical_gap_id::text, ''), dedupe_signature, reported_severity,
			       agent_run_id, task_kind, category, summary, observed, expected,
			       knowledge_refs, evidence_refs, trace_ref, client_event_id
			FROM gap_reports
			ORDER BY created_at DESC
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			gap, err := scanGap(rows)
			if err != nil {
				return err
			}
			out = append(out, gap)
		}
		return rows.Err()
	})
	if out == nil {
		out = []domain.Gap{}
	}
	return out, err
}

// Transition moves a gap and appends an event.
func (s *Service) Transition(ctx context.Context, workspaceID, id, to, actor, reason string) (domain.Gap, error) {
	if actor == "" || reason == "" {
		return domain.Gap{}, errors.New("actor and reason are required")
	}
	var gap domain.Gap
	err := s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		current, err := loadGap(ctx, tx, id)
		if err != nil {
			return err
		}
		if !domain.CanTransition(current.State, to) {
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `
			UPDATE gap_reports SET state = $2, updated_at = now() WHERE id = $1
		`, id, to); err != nil {
			return err
		}
		if err := insertEvent(ctx, tx, workspaceID, id, current.State, to, actor, reason); err != nil {
			return err
		}
		gap, err = loadGap(ctx, tx, id)
		return err
	})
	return gap, err
}

// Feedback stores an observation.
func (s *Service) Feedback(ctx context.Context, fb domain.Feedback) (string, error) {
	if err := domain.ValidateWorkspaceID(fb.WorkspaceID); err != nil {
		return "", err
	}
	if fb.KnowledgeURI == "" || fb.Revision == "" || fb.Observation == "" {
		return "", errors.New("feedback is missing a required field")
	}
	if findings := policy.ScanSecrets("observation", fb.Observation); len(findings) > 0 {
		if err := s.auditRejection(ctx, fb.WorkspaceID, "feedback", findings); err != nil {
			return "", err
		}
		return "", ErrSecret
	}
	id := uuid.NewString()
	err := s.DB.WithWorkspace(ctx, fb.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO feedback (id, workspace_id, knowledge_uri, revision, observation, agent_run_id)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
		`, id, fb.WorkspaceID, fb.KnowledgeURI, fb.Revision, fb.Observation, fb.AgentRunID)
		return err
	})
	return id, err
}

// UnreadFeedback returns observations the curator has not consumed.
func (s *Service) UnreadFeedback(ctx context.Context, workspaceID string) ([]string, error) {
	var out []string
	err := s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT observation FROM feedback
			WHERE workspace_id = $1 AND consumed_at IS NULL
			ORDER BY created_at
		`, workspaceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				return err
			}
			out = append(out, text)
		}
		return rows.Err()
	})
	return out, err
}

// ConsumeFeedback marks unread observations as read by a proposal.
func (s *Service) ConsumeFeedback(ctx context.Context, workspaceID string) error {
	return s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE feedback SET consumed_at = now()
			WHERE workspace_id = $1 AND consumed_at IS NULL
		`, workspaceID)
		return err
	})
}

// Events returns the append-only history of a gap.
func (s *Service) Events(ctx context.Context, workspaceID, id string) ([]Event, error) {
	var out []Event
	err := s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT coalesce(from_state, ''), to_state, actor, reason
			FROM gap_events WHERE gap_id = $1 ORDER BY created_at
		`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var event Event
			if err := rows.Scan(&event.From, &event.To, &event.Actor, &event.Reason); err != nil {
				return err
			}
			out = append(out, event)
		}
		return rows.Err()
	})
	return out, err
}

// AuditContains reports whether any audit payload in the workspace contains needle.
// Tests use it to prove a secret was not stored.
func (s *Service) AuditContains(ctx context.Context, workspaceID, needle string) (bool, error) {
	found := false
	err := s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		err := tx.QueryRow(ctx, `
			SELECT count(*) FROM audit_events
			WHERE details::text LIKE '%' || $1 || '%'
		`, needle).Scan(&n)
		found = n > 0
		return err
	})
	return found, err
}

func (s *Service) auditRejection(ctx context.Context, workspaceID, actor string, findings []policy.Finding) error {
	payload := map[string]any{
		"redacted": true,
		"findings": findings,
	}
	details, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	raw := string(details)
	for _, finding := range findings {
		if strings.Contains(raw, finding.Rule) && strings.Contains(raw, "AKIA") {
			return errors.New("refusing to write an audit record that still contains a secret")
		}
	}
	return s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := audit.Append(ctx, tx, workspaceID, audit.Event{
			Actor: actor, Action: "reject-secret", ObjectType: "gap_report", Details: payload,
		})
		return err
	})
}

func scanReport(report domain.GapReport) []policy.Finding {
	fields := []struct{ name, value string }{
		{"summary", report.Summary},
		{"observed", report.Observed},
		{"expected", report.Expected},
		{"task_kind", report.TaskKind},
		{"agent_run_id", report.AgentRunID},
		{"client_event_id", report.ClientEventID},
	}
	var out []policy.Finding
	for _, field := range fields {
		out = append(out, policy.ScanSecrets(field.name, field.value)...)
	}
	for _, ref := range append(append([]string{}, report.KnowledgeRefs...), report.EvidenceRefs...) {
		out = append(out, policy.ScanSecrets("ref", ref)...)
	}
	return out
}

func signature(report domain.GapReport) string {
	refs := append([]string{}, report.KnowledgeRefs...)
	sort.Strings(refs)
	payload := strings.Join([]string{
		report.WorkspaceID,
		report.Category,
		normalize(report.Observed),
		normalize(report.Expected),
		strings.Join(refs, ","),
	}, "\n")
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func findIdempotent(ctx context.Context, tx pgx.Tx, workspaceID, agentID, clientEventID string) (domain.Gap, error) {
	row := tx.QueryRow(ctx, `
		SELECT id FROM gap_reports
		WHERE agent_id = $1 AND client_event_id = $2 AND workspace_id = $3
	`, agentID, clientEventID, workspaceID)
	var id string
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Gap{}, ErrNotFound
		}
		return domain.Gap{}, err
	}
	return loadGap(ctx, tx, id)
}

func findOpen(ctx context.Context, tx pgx.Tx, sig string) (domain.Gap, error) {
	row := tx.QueryRow(ctx, `
		SELECT id FROM gap_reports
		WHERE dedupe_signature = $1
		  AND canonical_gap_id IS NULL
		  AND state IN ('new','triaged','investigating','proposal-ready','awaiting-review','accepted','published')
		FOR UPDATE
	`, sig)
	var id string
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Gap{}, ErrNotFound
		}
		return domain.Gap{}, err
	}
	return loadGap(ctx, tx, id)
}

func insertGap(ctx context.Context, tx pgx.Tx, gap domain.Gap, agentID string) error {
	report := gap.Report
	if report.KnowledgeRefs == nil {
		report.KnowledgeRefs = []string{}
	}
	if report.EvidenceRefs == nil {
		report.EvidenceRefs = []string{}
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO gap_reports (
			id, workspace_id, agent_id, agent_run_id, task_kind, category, summary, observed, expected,
			knowledge_refs, evidence_refs, trace_ref, client_event_id, reported_severity, state,
			dedupe_signature, failure_signature, canonical_gap_id, source_revision
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9,
			$10, $11, $12, $13, $14, $15,
			$16, $16, NULLIF($17, '')::uuid, ''
		)
	`, gap.ID, report.WorkspaceID, agentID, report.AgentRunID, report.TaskKind, report.Category, report.Summary, report.Observed, report.Expected,
		report.KnowledgeRefs, report.EvidenceRefs, report.TraceRef, report.ClientEventID, report.Severity, gap.State,
		gap.DedupeSignature, gap.CanonicalID)
	return err
}

func insertEvent(ctx context.Context, tx pgx.Tx, workspaceID, gapID, from, to, actor, reason string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO gap_events (id, workspace_id, gap_id, from_state, to_state, actor, reason)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7)
	`, uuid.NewString(), workspaceID, gapID, from, to, actor, reason)
	return err
}

func loadGap(ctx context.Context, tx pgx.Tx, id string) (domain.Gap, error) {
	row := tx.QueryRow(ctx, `
		SELECT id, state, coalesce(canonical_gap_id::text, ''), dedupe_signature, reported_severity,
		       agent_run_id, task_kind, category, summary, observed, expected,
		       knowledge_refs, evidence_refs, trace_ref, client_event_id
		FROM gap_reports WHERE id = $1
	`, id)
	return scanGap(row)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanGap(row scanner) (domain.Gap, error) {
	var gap domain.Gap
	var trace *string
	err := row.Scan(&gap.ID, &gap.State, &gap.CanonicalID, &gap.DedupeSignature, &gap.ReportedSeverity,
		&gap.Report.AgentRunID, &gap.Report.TaskKind, &gap.Report.Category, &gap.Report.Summary, &gap.Report.Observed, &gap.Report.Expected,
		&gap.Report.KnowledgeRefs, &gap.Report.EvidenceRefs, &trace, &gap.Report.ClientEventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Gap{}, ErrNotFound
	}
	if err != nil {
		return domain.Gap{}, err
	}
	gap.Report.TraceRef = trace
	gap.Report.Severity = gap.ReportedSeverity
	return gap, nil
}

// Linked returns the canonical gap and every submission that points at it.
func (s *Service) Linked(ctx context.Context, workspaceID, id string) (domain.Gap, []domain.Gap, error) {
	var canonical domain.Gap
	var related []domain.Gap
	err := s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		gap, err := loadGap(ctx, tx, id)
		if err != nil {
			return err
		}
		canonicalID := gap.ID
		if gap.CanonicalID != "" {
			canonicalID = gap.CanonicalID
		}
		canonical, err = loadGap(ctx, tx, canonicalID)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id, state, coalesce(canonical_gap_id::text, ''), dedupe_signature, reported_severity,
			       agent_run_id, task_kind, category, summary, observed, expected,
			       knowledge_refs, evidence_refs, trace_ref, client_event_id
			FROM gap_reports WHERE canonical_gap_id = $1 ORDER BY created_at
		`, canonicalID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			item, err := scanGap(rows)
			if err != nil {
				return err
			}
			related = append(related, item)
		}
		return rows.Err()
	})
	return canonical, related, err
}
