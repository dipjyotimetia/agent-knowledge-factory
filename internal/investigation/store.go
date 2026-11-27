// Package investigation owns the durable investigation queue and operation leases.
package investigation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/audit"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrMismatch = errors.New("investigation workspace, policy or attempt mismatch")
	ErrBusy     = errors.New("investigation lease is already held")
	ErrStale    = errors.New("investigation lease expired")
	ErrTerminal = errors.New("investigation is terminal")
	ErrBudget   = errors.New("investigation operation exhausted its budget")
)

type Input struct {
	WorkspaceID   string
	GapID         string
	AttemptID     string
	BaseReleaseID string
	PolicyVersion string
}

type Store struct {
	DB            *postgres.DB
	Workspace     string
	PolicyVersion string
	MaxConcurrent int
}

type Attempt struct {
	Input
	State, ProposalID, Result string
}

func (s Store) input(gap, id, base string) Input {
	return Input{s.Workspace, gap, id, base, s.PolicyVersion}
}

func (s Store) Enqueue(ctx context.Context, gap string) (Input, error) {
	var result Input
	if _, err := uuid.Parse(gap); err != nil {
		return result, ErrMismatch
	}
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.lockQueue(ctx, tx); err != nil {
			return err
		}
		var canonical, state string
		err := tx.QueryRow(ctx, `SELECT g.id::text,g.state FROM gap_reports r JOIN gap_reports g
            ON (g.workspace_id,g.id)=(r.workspace_id,coalesce(r.canonical_gap_id,r.id))
            WHERE r.workspace_id=$1 AND r.id=$2 FOR UPDATE OF g`, s.Workspace, gap).Scan(&canonical, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMismatch
		}
		if err != nil {
			return err
		}
		var id, base, policyVersion string
		err = tx.QueryRow(ctx, `SELECT id::text,coalesce(base_release_id::text,''),policy_version
            FROM investigation_attempts WHERE workspace_id=$1 AND gap_id=$2 AND state IN ('queued','running','review','parked')`,
			s.Workspace, canonical).Scan(&id, &base, &policyVersion)
		if err == nil {
			result = Input{s.Workspace, canonical, id, base, policyVersion}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM investigation_attempts WHERE workspace_id=$1 AND gap_id=$2
            AND result IN ('pending-pr-failed','pending-pr-cancelled'))`, s.Workspace, canonical).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return ErrTerminal
		}
		switch state {
		case "new", "triaged", "investigating", "proposal-ready":
		default:
			return ErrTerminal
		}
		if state == "proposal-ready" {
			return ErrTerminal
		}
		id = uuid.NewString()
		result = s.input(canonical, id, "")
		_, err = tx.Exec(ctx, `INSERT INTO investigation_attempts(workspace_id,id,gap_id,policy_version,state)
            VALUES($1,$2,$3,$4,'queued')`, s.Workspace, id, canonical, s.PolicyVersion)
		return err
	})
	return result, err
}

func (s Store) lockQueue(ctx context.Context, tx pgx.Tx) error {
	var ws string
	return tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, s.Workspace).Scan(&ws)
}

func (s Store) Starts(ctx context.Context) ([]Input, error) {
	var inputs []Input
	maxConcurrent := s.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		var base string
		var held bool
		if err := tx.QueryRow(ctx, `SELECT coalesce(active_release_id::text,''),publication_hold
            FROM workspaces WHERE id=$1 FOR UPDATE`, s.Workspace).Scan(&base, &held); err != nil {
			return err
		}
		if !held && base != "" {
			var active int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM investigation_attempts WHERE workspace_id=$1 AND state IN ('running','review')`, s.Workspace).Scan(&active); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE investigation_attempts SET state=CASE WHEN legacy_review THEN 'review' ELSE 'running' END,
                review_at=CASE WHEN legacy_review THEN now() ELSE review_at END,base_release_id=$2,
                target_case_ids=ARRAY(SELECT c.case_id FROM gap_retrieval_cases c WHERE
                    (c.workspace_id,c.gap_id)=(investigation_attempts.workspace_id,investigation_attempts.gap_id) ORDER BY c.case_id) WHERE (workspace_id,id) IN (
                SELECT workspace_id,id FROM investigation_attempts WHERE workspace_id=$1 AND state='queued'
                AND policy_version=$3 AND base_release_id IS DISTINCT FROM $2::uuid ORDER BY created_at,id LIMIT $4)`, s.Workspace, base, s.PolicyVersion, max(0, maxConcurrent-active)); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `SELECT gap_id::text,id::text,base_release_id::text FROM investigation_attempts
            WHERE workspace_id=$1 AND state IN ('running','review','parked') AND started_at IS NULL AND policy_version=$2 AND base_release_id IS NOT NULL
            ORDER BY created_at,id LIMIT 100`, s.Workspace, s.PolicyVersion)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var gap, id, release string
			if err := rows.Scan(&gap, &id, &release); err != nil {
				return err
			}
			inputs = append(inputs, s.input(gap, id, release))
		}
		return rows.Err()
	})
	return inputs, err
}

func (s Store) Started(ctx context.Context, in Input) error {
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := s.load(ctx, tx, in, false); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE investigation_attempts SET started_at=coalesce(started_at,now()) WHERE workspace_id=$1 AND id=$2`, s.Workspace, in.AttemptID)
		return err
	})
}

func (s Store) load(ctx context.Context, tx pgx.Tx, in Input, lock bool) (Attempt, error) {
	out := Attempt{Input: in}
	if in.WorkspaceID != s.Workspace || in.PolicyVersion != s.PolicyVersion {
		return out, ErrMismatch
	}
	for _, id := range []string{in.GapID, in.AttemptID, in.BaseReleaseID} {
		if _, err := uuid.Parse(id); err != nil {
			return out, ErrMismatch
		}
	}
	query := `SELECT state,coalesce(proposal_id::text,''),result FROM investigation_attempts
        WHERE workspace_id=$1 AND id=$2 AND gap_id=$3 AND base_release_id=$4 AND policy_version=$5`
	if lock {
		query += " FOR UPDATE"
	}
	err := tx.QueryRow(ctx, query, s.Workspace, in.AttemptID, in.GapID, in.BaseReleaseID, in.PolicyVersion).Scan(&out.State, &out.ProposalID, &out.Result)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrMismatch
	}
	return out, err
}

func (s Store) Get(ctx context.Context, in Input) (Attempt, error) {
	var out Attempt
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.load(ctx, tx, in, false)
		return err
	})
	return out, err
}

func (s Store) Page(ctx context.Context, after string) ([]string, error) {
	var out []string
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text FROM gap_reports g WHERE workspace_id=$1 AND id>coalesce(NULLIF($2,'')::uuid,'00000000-0000-0000-0000-000000000000')
            AND canonical_gap_id IS NULL AND state IN ('new','triaged','investigating')
            AND NOT EXISTS (SELECT 1 FROM investigation_attempts a WHERE a.workspace_id=g.workspace_id AND a.gap_id=g.id)
            ORDER BY id LIMIT 50`, s.Workspace, after)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	return out, err
}

func (s Store) BaseSHA(ctx context.Context, in Input) (string, error) {
	var sha string
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := s.load(ctx, tx, in, false); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT git_sha FROM knowledge_releases WHERE workspace_id=$1 AND id=$2`, s.Workspace, in.BaseReleaseID).Scan(&sha)
	})
	return sha, err
}

func (s Store) Transition(ctx context.Context, in Input, from, to, reason string) error {
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		attempt, err := s.load(ctx, tx, in, true)
		if err != nil {
			return err
		}
		if attempt.State == to {
			return nil
		}
		if attempt.State != from {
			return ErrTerminal
		}
		if _, err := tx.Exec(ctx, `UPDATE investigation_attempts SET state=$3,result=$4 WHERE (workspace_id,id)=($1,$2)`, s.Workspace, in.AttemptID, to, reason); err != nil {
			return err
		}
		_, err = audit.Append(ctx, tx, s.Workspace, audit.Event{Actor: "investigation", Action: "attempt-" + to,
			ObjectType: "attempt", ObjectID: in.AttemptID, Details: map[string]string{"reason": reason}})
		return err
	})
}

func (s Store) Review(ctx context.Context, gap string) (Input, error) {
	var in Input
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		var id, base string
		err := tx.QueryRow(ctx, `SELECT id::text,base_release_id::text FROM investigation_attempts WHERE workspace_id=$1
            AND gap_id::text=$2 AND policy_version=$3 AND state IN ('review','parked')`, s.Workspace, gap, s.PolicyVersion).Scan(&id, &base)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMismatch
		}
		if err != nil {
			return err
		}
		in = s.input(gap, id, base)
		return nil
	})
	return in, err
}

func (s Store) Resume(ctx context.Context, id string) (Input, error) {
	var in Input
	if _, err := uuid.Parse(id); err != nil {
		return in, ErrMismatch
	}
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.lockQueue(ctx, tx); err != nil {
			return err
		}
		var gap, base, state string
		err := tx.QueryRow(ctx, `SELECT gap_id::text,base_release_id::text,state FROM investigation_attempts WHERE (workspace_id,id)=($1,$2)
            AND state IN ('parked','review') AND base_release_id IS NOT NULL AND policy_version=$3 FOR UPDATE`, s.Workspace, id, s.PolicyVersion).Scan(&gap, &base, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMismatch
		}
		if err != nil {
			return err
		}
		in = s.input(gap, id, base)
		if state == "review" {
			return nil
		}
		maxConcurrent := s.MaxConcurrent
		if maxConcurrent <= 0 {
			maxConcurrent = 4
		}
		var active int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM investigation_attempts WHERE workspace_id=$1 AND state IN ('running','review')`, s.Workspace).Scan(&active); err != nil {
			return err
		}
		if active >= maxConcurrent {
			return ErrBusy
		}
		_, err = tx.Exec(ctx, `UPDATE investigation_attempts SET state='review',review_at=now(),reminded_at=NULL,result=''
            WHERE (workspace_id,id)=($1,$2)`, s.Workspace, id)
		if err != nil {
			return err
		}
		_, err = audit.Append(ctx, tx, s.Workspace, audit.Event{Actor: "operator", Action: "attempt-resumed", ObjectType: "attempt", ObjectID: id})
		return err
	})
	return in, err
}

func (s Store) Remind(ctx context.Context, in Input) error {
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		attempt, err := s.load(ctx, tx, in, true)
		if err != nil {
			return err
		}
		if attempt.State != "review" {
			return nil
		}
		tag, err := tx.Exec(ctx, `UPDATE investigation_attempts SET reminded_at=now() WHERE (workspace_id,id)=($1,$2) AND reminded_at IS NULL`, s.Workspace, in.AttemptID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = audit.Append(ctx, tx, s.Workspace, audit.Event{Actor: "investigation", Action: "review-reminder", ObjectType: "attempt", ObjectID: in.AttemptID})
		return err
	})
}

func WorkflowID(in Input) string {
	return fmt.Sprintf("curate-%s-%s-%s", in.WorkspaceID, in.GapID, in.AttemptID)
}

type ReviewClock struct {
	State      string
	At         time.Time
	Reminded   bool
	ProposalID string
}

func (s Store) ReviewClock(ctx context.Context, in Input) (ReviewClock, error) {
	var result ReviewClock
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		attempt, err := s.load(ctx, tx, in, false)
		if err != nil {
			return err
		}
		result.State, result.ProposalID = attempt.State, attempt.ProposalID
		if attempt.State != "review" && attempt.State != "parked" {
			return nil
		}
		return tx.QueryRow(ctx, `SELECT review_at,reminded_at IS NOT NULL FROM investigation_attempts
            WHERE (workspace_id,id)=($1,$2)`, s.Workspace, in.AttemptID).Scan(&result.At, &result.Reminded)
	})
	return result, err
}

// End records cancellation or an exhausted activity while preserving a pending PR.
func (s Store) End(ctx context.Context, in Input, state string) error {
	if state != "cancelled" && state != "failed" {
		return ErrMismatch
	}
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		attempt, err := s.load(ctx, tx, in, true)
		if err != nil {
			return err
		}
		switch attempt.State {
		case "running", "review", "parked":
		default:
			return nil
		}
		var mayHavePR bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operation_ledger WHERE workspace_id=$1 AND attempt_id=$2 AND kind='pr')`,
			s.Workspace, in.AttemptID).Scan(&mayHavePR); err != nil {
			return err
		}
		mayHavePR = mayHavePR || attempt.State == "review" || attempt.State == "parked"
		if !mayHavePR && attempt.ProposalID == "" {
			if err := s.releaseFeedback(ctx, tx, in.AttemptID); err != nil {
				return err
			}
		}
		reason := "activity-" + state
		if mayHavePR {
			reason = "pending-pr-" + state
		} else {
			var gapState string
			if err := tx.QueryRow(ctx, `SELECT state FROM gap_reports WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, s.Workspace, in.GapID).Scan(&gapState); err != nil {
				return err
			}
			if domain.CanTransition(gapState, state) {
				if _, err := tx.Exec(ctx, `UPDATE gap_reports SET state=$3,updated_at=now() WHERE workspace_id=$1 AND id=$2`, s.Workspace, in.GapID, state); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO gap_events(id,workspace_id,gap_id,from_state,to_state,actor,reason,correlation_id)
                    VALUES($1,$2,$3,$4,$5,'investigation',$6,$7)`, uuid.NewString(), s.Workspace, in.GapID, gapState, state, reason, in.AttemptID); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE investigation_attempts SET state=$3,result=$4 WHERE (workspace_id,id)=($1,$2)`,
			s.Workspace, in.AttemptID, state, reason); err != nil {
			return err
		}
		_, err = audit.Append(ctx, tx, s.Workspace, audit.Event{Actor: "investigation", Action: "attempt-" + state,
			ObjectType: "attempt", ObjectID: in.AttemptID, Details: map[string]string{"reason": reason}})
		return err
	})
}
