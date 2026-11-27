package investigation

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/audit"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/policy"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/jackc/pgx/v5"
)

const leaseSeconds = 45

type Lease struct {
	Input
	Kind  string
	Token int64
	Done  bool
}

func (s Store) Acquire(ctx context.Context, in Input, kind, digest string) (Lease, error) {
	lease := Lease{Input: in, Kind: kind}
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		attempt, err := s.load(ctx, tx, in, true)
		if err != nil {
			return err
		}
		var status, previous string
		var active bool
		err = tx.QueryRow(ctx, `SELECT status,request_digest,lease_token,coalesce(lease_until>clock_timestamp(),false) FROM operation_ledger
            WHERE (workspace_id,attempt_id,kind,operation_key)=($1,$2,$3,'step')`, s.Workspace, in.AttemptID, kind).
			Scan(&status, &previous, &lease.Token, &active)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if previous != "" && previous != digest {
			return ErrMismatch
		}
		if status == "completed" {
			lease.Done = true
			return nil
		}
		if attempt.State != "running" || status == "failed" {
			return ErrTerminal
		}
		if active {
			return ErrBusy
		}
		err = tx.QueryRow(ctx, `INSERT INTO operation_ledger(workspace_id,attempt_id,kind,operation_key,request_digest,status,attempt_count,lease_token,lease_until)
            VALUES($1,$2,$3,'step',$4,'pending',1,1,clock_timestamp()+make_interval(secs=>$5))
            ON CONFLICT(workspace_id,attempt_id,kind,operation_key) DO UPDATE SET
                lease_token=operation_ledger.lease_token+1,attempt_count=operation_ledger.attempt_count+1,
                lease_until=excluded.lease_until WHERE operation_ledger.attempt_count<4 RETURNING lease_token`,
			s.Workspace, in.AttemptID, kind, digest, leaseSeconds).Scan(&lease.Token)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBudget
		}
		return err
	})
	return lease, err
}

func (s Store) Fence(ctx context.Context, lease Lease) context.Context {
	return postgres.WithTransactionFence(ctx, func(ctx context.Context, tx pgx.Tx, workspace string) error {
		if workspace != s.Workspace || lease.WorkspaceID != s.Workspace {
			return ErrMismatch
		}
		attempt, err := s.load(ctx, tx, lease.Input, true)
		if err != nil {
			return err
		}
		if attempt.State != "running" {
			return ErrStale
		}
		var token int64
		err = tx.QueryRow(ctx, `SELECT lease_token FROM operation_ledger WHERE (workspace_id,attempt_id,kind,operation_key)=($1,$2,$3,'step')
            AND status='pending' AND lease_token=$4 AND lease_until>clock_timestamp() FOR UPDATE`,
			s.Workspace, lease.AttemptID, lease.Kind, lease.Token).Scan(&token)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStale
		}
		return err
	})
}

func (s Store) Renew(ctx context.Context, lease Lease) error {
	return s.DB.WithWorkspace(s.Fence(ctx, lease), s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE operation_ledger SET lease_until=clock_timestamp()+make_interval(secs=>$5)
            WHERE (workspace_id,attempt_id,kind,operation_key)=($1,$2,$3,'step') AND lease_token=$4`,
			s.Workspace, lease.AttemptID, lease.Kind, lease.Token, leaseSeconds)
		return err
	})
}

func (s Store) Release(ctx context.Context, lease Lease, code string, terminal bool) error {
	return s.DB.WithWorkspace(s.Fence(ctx, lease), s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE operation_ledger SET lease_until=NULL,error_code=$5,status=CASE WHEN $6 THEN 'failed' ELSE 'pending' END
            WHERE (workspace_id,attempt_id,kind,operation_key)=($1,$2,$3,'step') AND lease_token=$4`,
			s.Workspace, lease.AttemptID, lease.Kind, lease.Token, code, terminal)
		return err
	})
}

func (s Store) Finish(ctx context.Context, lease Lease, state, result, proposal string) error {
	return s.DB.WithWorkspace(s.Fence(ctx, lease), s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE operation_ledger SET status='completed',lease_until=NULL,error_code=''
            WHERE (workspace_id,attempt_id,kind,operation_key)=($1,$2,$3,'step') AND lease_token=$4`,
			s.Workspace, lease.AttemptID, lease.Kind, lease.Token); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE investigation_attempts SET state=$3,result=$4,proposal_id=coalesce(NULLIF($5,'')::uuid,proposal_id),
            review_at=CASE WHEN $3='review' THEN now() ELSE review_at END WHERE (workspace_id,id)=($1,$2)`,
			s.Workspace, lease.AttemptID, state, result, proposal)
		if err != nil || state == "running" {
			return err
		}
		_, err = audit.Append(ctx, tx, s.Workspace, audit.Event{Actor: "investigation", Action: "attempt-" + state,
			ObjectType: "attempt", ObjectID: lease.AttemptID, Details: map[string]string{"reason": result}})
		return err
	})
}

func (s Store) ReserveModel(ctx context.Context, in Input, digest string, limit int) error {
	if limit < 1 {
		return ErrBudget
	}
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO operation_ledger(workspace_id,attempt_id,kind,operation_key,request_digest,status,attempt_count)
            VALUES($1,$2,'model','advice',$3,'pending',1)
            ON CONFLICT(workspace_id,attempt_id,kind,operation_key) DO UPDATE SET attempt_count=operation_ledger.attempt_count+1
            WHERE operation_ledger.request_digest=$3 AND operation_ledger.attempt_count<$4 AND operation_ledger.status='pending'`,
			s.Workspace, in.AttemptID, digest, limit)
		if err == nil && tag.RowsAffected() != 1 {
			return ErrBudget
		}
		return err
	})
}

// ModelResult returns a previously validated response for this exact request.
func (s Store) ModelResult(ctx context.Context, in Input, digest string) (curator.Proposal, bool, error) {
	var result curator.Proposal
	var raw []byte
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		var previous string
		err := tx.QueryRow(ctx, `SELECT request_digest,result FROM operation_ledger WHERE (workspace_id,attempt_id,kind,operation_key)=
            ($1,$2,'model','advice')`, s.Workspace, in.AttemptID).Scan(&previous, &raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err == nil && previous != digest {
			return ErrMismatch
		}
		return err
	})
	if err != nil || len(raw) == 0 {
		return result, false, err
	}
	err = json.Unmarshal(raw, &result)
	return result, err == nil, err
}

func (s Store) CompleteModel(ctx context.Context, in Input, digest string, result curator.Proposal) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if len(raw) > 128<<10 || policy.Redact(string(raw)) != string(raw) {
		return ErrMismatch
	}
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE operation_ledger SET status='completed',result=$4 WHERE
            (workspace_id,attempt_id,kind,operation_key)=($1,$2,'model','advice') AND request_digest=$3 AND status='pending'`,
			s.Workspace, in.AttemptID, digest, raw)
		if err == nil && tag.RowsAffected() != 1 {
			return ErrMismatch
		}
		return err
	})
}

func (s Store) PriorPR(ctx context.Context, in Input, digest, branch string) (bool, error) {
	var previous, ref string
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT request_digest,external_ref FROM operation_ledger
            WHERE (workspace_id,attempt_id,kind,operation_key)=($1,$2,'pr','create')`, s.Workspace, in.AttemptID).Scan(&previous, &ref)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return false, err
	}
	if previous != "" && (previous != digest || ref != branch) {
		return false, ErrMismatch
	}
	return previous != "", nil
}

func (s Store) ReservePR(ctx context.Context, in Input, digest, branch, receipt string) error {
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO operation_ledger(workspace_id,attempt_id,kind,operation_key,request_digest,status,external_ref,attempt_count)
            VALUES($1,$2,'pr','create',$3,'pending',$4,1)`, s.Workspace, in.AttemptID, digest, branch); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE investigation_attempts SET evaluation_receipt_id=$3 WHERE (workspace_id,id)=($1,$2)`,
			s.Workspace, in.AttemptID, receipt)
		return err
	})
}

func (s Store) CompletePR(ctx context.Context, in Input) error {
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE operation_ledger SET status='completed' WHERE (workspace_id,attempt_id,kind,operation_key)=($1,$2,'pr','create')`,
			s.Workspace, in.AttemptID)
		return err
	})
}
