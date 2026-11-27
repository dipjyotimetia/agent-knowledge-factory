package traces

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/auth"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/policy"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrNotFound = errors.New("trace not found")

type Store struct {
	DB *postgres.DB
}

// Import persists only the normalized transcript. The owner comes from the
// trusted workspace configuration, and the operator is never taken from input.
func (s Store) Import(ctx context.Context, operator auth.Principal, runID, owner string, raw []byte) (string, error) {
	if operator.Role != auth.RoleCurator || !domain.ValidActorID(operator.ActorID) || !domain.ValidActorID(owner) ||
		len(runID) == 0 || len(runID) > 200 || policy.Redact(runID) != runID {
		return "", ErrNotFound
	}
	doc, err := ImportJSON(raw)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return "", ErrInvalid
	}
	digest := sha256.Sum256(payload)
	id := uuid.NewString()
	err = s.DB.WithWorkspace(ctx, operator.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO trace_records (workspace_id,id,owner_actor,agent_run_id,schema_version,content_digest,sanitised_payload,created_at,expires_at)
			SELECT id,$2,$3,$4,1,$5,$6,statement_timestamp(),statement_timestamp() + make_interval(days => retention_days)
			FROM workspaces WHERE id=$1`, operator.WorkspaceID, id, owner, runID, hex.EncodeToString(digest[:]), payload)
		if err == nil && tag.RowsAffected() != 1 {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return "", err
	}
	return "trace:" + id, nil
}

// AuthorizeLink runs in the same transaction as gap insertion.
func AuthorizeLink(ctx context.Context, tx pgx.Tx, principal auth.Principal, runID, id string) error {
	if !domain.ValidActorID(principal.ActorID) ||
		(principal.Role != auth.RoleCurator && principal.Role != auth.RoleAgent) {
		return ErrNotFound
	}
	var found bool
	err := tx.QueryRow(ctx, `
		SELECT true FROM trace_records
		WHERE workspace_id=$1 AND id=$2 AND agent_run_id=$3
		  AND (owner_actor=$4 OR $5) AND status='ready' AND expires_at>clock_timestamp()
		FOR KEY SHARE`, principal.WorkspaceID, id, runID, principal.ActorID, principal.Role == auth.RoleCurator).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// GetForInvestigation derives the link and authority from the persisted
// attempt and records a fixed outcome under the activity's transaction fence.
func (s Store) GetForInvestigation(ctx context.Context, workspace, attemptID string) (Transcript, error) {
	var doc Transcript
	err := s.DB.WithWorkspace(ctx, workspace, func(ctx context.Context, tx pgx.Tx) error {
		var ref *string
		var raw []byte
		err := tx.QueryRow(ctx, `
			SELECT g.trace_ref,t.sanitised_payload
			FROM investigation_attempts a
			JOIN gap_reports g ON (g.workspace_id,g.id)=(a.workspace_id,a.gap_id)
			LEFT JOIN trace_records t
			  ON t.workspace_id=g.workspace_id AND 'trace:'||t.id::text=g.trace_ref
			  AND t.agent_run_id=g.agent_run_id AND (t.owner_actor=g.trace_actor OR g.trace_curator)
			  AND t.status='ready' AND t.expires_at>clock_timestamp()
			WHERE a.workspace_id=$1 AND a.id=$2 AND a.state='running'`, workspace, attemptID).Scan(&ref, &raw)
		if err != nil || ref == nil {
			return err
		}
		outcome := "unavailable"
		if len(raw) != 0 && json.Unmarshal(raw, &doc) == nil {
			outcome = "used"
		} else {
			doc = Transcript{}
		}
		_, err = tx.Exec(ctx, `UPDATE investigation_attempts SET trace_outcome=$3 WHERE workspace_id=$1 AND id=$2`,
			workspace, attemptID, outcome)
		return err
	})
	return doc, err
}
