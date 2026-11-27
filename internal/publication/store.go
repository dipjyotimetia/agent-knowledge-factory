// Package publication validates and installs complete Git knowledge releases.
package publication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/audit"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/evaluations"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrUnmapped = errors.New("unmapped PR or gap")
	ErrIdentity = errors.New("publication identity mismatch")
	ErrHeld     = errors.New("publication is held pending KF-24 and KF-25")
	ErrStale    = errors.New("publication lease or baseline changed")
	ErrRejected = errors.New("publication candidate rejected")
)

type Service struct {
	DB           *postgres.DB
	Index        *index.Service
	Forge        publish.Forge
	Workspace    string
	Identity     publish.Identity
	CasesPath    string
	PolicyDigest string
	Sources      func(context.Context) (EvidenceSet, error)
}

type Request struct {
	Trigger     string
	DeliveryID  string
	ObservedSHA string
	PRNumber    int
	GapID       string
}

type operation struct {
	id, trigger, proposal string
	token                 int64
}

type Baseline struct {
	SHA, ReleaseID string
	Generation     int64
	Held           bool
}

type Receipt struct {
	CandidateID string
	Baseline
	SHA, TreeDigest, PolicyDigest, DatasetDigest, EvaluatorDigest string
	Outcome, Reason                                               string
	Before, After                                                 evaluations.Report
}

func (s *Service) RequestRelease(ctx context.Context, req Request) (string, error) {
	if s.Identity.Repository == "" || s.Identity.Branch == "" || s.Identity.Installation == "" {
		return "", ErrIdentity
	}
	if len(req.DeliveryID) > 128 || (req.ObservedSHA != "" && !shaPattern.MatchString(req.ObservedSHA)) {
		return "", ErrIdentity
	}
	var id string
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		var repository string
		if err := tx.QueryRow(ctx, `SELECT knowledge_repo FROM workspaces WHERE id=$1 FOR UPDATE`, s.Workspace).Scan(&repository); err != nil {
			return err
		}
		if !strings.EqualFold(repository, s.Identity.Repository) {
			return ErrIdentity
		}
		if req.DeliveryID != "" {
			err := tx.QueryRow(ctx, `SELECT id::text FROM publication_operations WHERE workspace_id=$1 AND delivery_id=$2`, s.Workspace, req.DeliveryID).Scan(&id)
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if req.Trigger == "reconcile" {
			err := tx.QueryRow(ctx, `SELECT id::text FROM publication_operations WHERE workspace_id=$1 AND trigger='reconcile'
                AND state IN ('queued','validating','staged')`, s.Workspace).Scan(&id)
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		var proposal string
		if req.Trigger == "pr-merge" || req.Trigger == "pr-head" {
			if req.PRNumber == 0 && req.GapID == "" {
				return ErrUnmapped
			}
			err := tx.QueryRow(ctx, `SELECT id::text FROM proposals WHERE workspace_id=$1 AND forge_repository=$2
                AND forge_installation=$3 AND forge_base_branch=$4 AND git_pr_number>0
                AND ($5=0 OR git_pr_number=$5) AND ($6='' OR $6=ANY(gap_ids::text[]))
                ORDER BY created_at DESC LIMIT 1`, s.Workspace, s.Identity.Repository, s.Identity.Installation,
				s.Identity.Branch, req.PRNumber, req.GapID).Scan(&proposal)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrUnmapped
			}
			if err != nil {
				return err
			}
		}
		if req.DeliveryID != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries(workspace_id,delivery_id) VALUES ($1,$2)
                ON CONFLICT DO NOTHING`, s.Workspace, req.DeliveryID); err != nil {
				return err
			}
		}
		id = uuid.NewString()
		_, err := tx.Exec(ctx, `INSERT INTO publication_operations(workspace_id,id,trigger,repository,branch,installation,
            observed_sha,proposal_id,delivery_id) VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,'')::uuid,NULLIF($9,''))`,
			s.Workspace, id, req.Trigger, s.Identity.Repository, s.Identity.Branch, s.Identity.Installation, req.ObservedSHA, proposal, req.DeliveryID)
		return err
	})
	return id, err
}

func (s *Service) Baseline(ctx context.Context) (Baseline, error) {
	var base Baseline
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT published_sha,coalesce(active_release_id::text,''),publication_generation,publication_hold
            FROM workspaces WHERE id=$1`, s.Workspace).Scan(&base.SHA, &base.ReleaseID, &base.Generation, &base.Held)
	})
	return base, err
}

func (s *Service) claim(ctx context.Context) (operation, error) {
	var op operation
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `UPDATE publication_operations SET state='validating',lease_token=lease_token+1,
            lease_until=now()+interval '5 minutes' WHERE (workspace_id,id) = (
                SELECT workspace_id,id FROM publication_operations WHERE workspace_id=$1
                AND repository=$2 AND branch=$3 AND installation=$4 AND available_at<=now()
                AND (state='queued' OR (state IN ('validating','staged') AND lease_until<=now()))
                ORDER BY available_at,created_at LIMIT 1 FOR UPDATE SKIP LOCKED
            ) RETURNING id::text, trigger,coalesce(proposal_id::text,''),lease_token`,
			s.Workspace, s.Identity.Repository, s.Identity.Branch, s.Identity.Installation).Scan(&op.id, &op.trigger, &op.proposal, &op.token)
	})
	return op, err
}

func (s *Service) lockOperation(ctx context.Context, tx pgx.Tx, op operation) error {
	if op.id == "" {
		return nil
	}
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM publication_operations WHERE workspace_id=$1 AND id=$2 AND lease_token=$3
        AND state IN ('validating','staged') AND lease_until>clock_timestamp() FOR UPDATE`, s.Workspace, op.id, op.token).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStale
	}
	return err
}

func (s *Service) finish(ctx context.Context, op operation, state, reason string) error {
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.lockOperation(ctx, tx, op); err != nil {
			return err
		}
		return s.finishTx(ctx, tx, op, state, reason)
	})
}

func (s *Service) finishTx(ctx context.Context, tx pgx.Tx, op operation, state, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE publication_operations SET state=$3,lease_until=NULL,available_at=now()+interval '15 seconds'
        WHERE workspace_id=$1 AND id=$2`, s.Workspace, op.id, state); err != nil {
		return err
	}
	_, err := audit.Append(ctx, tx, s.Workspace, audit.Event{Actor: "publication", Action: "publication-" + state, ObjectID: op.id, ObjectType: "publication",
		Details: map[string]any{"reason": reason}})
	return err
}

func (s *Service) saveReceipt(ctx context.Context, op operation, proposal string, receipt Receipt) error {
	raw, err := json.Marshal([2]evaluations.Report{receipt.Before, receipt.After})
	if err != nil {
		return err
	}
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.lockOperation(ctx, tx, op); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO publication_receipts(workspace_id,candidate_id,operation_id,proposal_id,lease_token,
            baseline_sha,generation,git_sha,tree_digest,policy_digest,dataset_digest,evaluator_digest,outcome,reason,results)
            VALUES ($1,$2,NULLIF($3,'')::uuid,NULLIF($4,'')::uuid,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			s.Workspace, receipt.CandidateID, op.id, proposal, op.token, receipt.Baseline.SHA, receipt.Generation, receipt.SHA,
			receipt.TreeDigest, receipt.PolicyDigest, receipt.DatasetDigest, receipt.EvaluatorDigest, receipt.Outcome, receipt.Reason, raw)
		if err != nil || op.id == "" {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE publication_operations SET state='staged' WHERE workspace_id=$1 AND id=$2`, s.Workspace, op.id)
		return err
	})
}

func (s *Service) Promote(ctx context.Context, candidateID string, expectedGeneration int64) (string, error) {
	return s.promote(ctx, operation{}, candidateID, expectedGeneration)
}

func (s *Service) promote(ctx context.Context, claim operation, candidateID string, expectedGeneration int64) (string, error) {
	var receipt Receipt
	var op operation
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(operation_id::text,''),lease_token,baseline_sha,generation,git_sha,tree_digest,
            policy_digest,dataset_digest,evaluator_digest,outcome FROM publication_receipts WHERE workspace_id=$1 AND candidate_id=$2`, s.Workspace, candidateID).
			Scan(&op.id, &op.token, &receipt.Baseline.SHA, &receipt.Generation, &receipt.SHA, &receipt.TreeDigest, &receipt.PolicyDigest,
				&receipt.DatasetDigest, &receipt.EvaluatorDigest, &receipt.Outcome)
	})
	if err != nil {
		return "", err
	}
	if op.id == "" || receipt.Outcome != "passed" || receipt.Generation != expectedGeneration || receipt.PolicyDigest != s.PolicyDigest {
		return "", ErrStale
	}
	if claim.id != "" {
		if claim.id != op.id {
			return "", ErrStale
		}
		op = claim
	}
	dataset, _, err := s.dataset()
	if err != nil {
		return "", err
	}
	if dataset != receipt.DatasetDigest || s.evaluator() != receipt.EvaluatorDigest {
		return "", ErrStale
	}
	head, err := s.head(ctx)
	if err != nil {
		return "", err
	}
	if head != receipt.SHA {
		ahead, err := s.Forge.IsAncestor(ctx, receipt.SHA, head)
		if err != nil {
			return "", err
		}
		if ahead {
			return "", publish.ErrStaleBase
		}
		return "", fmt.Errorf("%w: default head diverged", ErrRejected)
	}
	var id string
	err = s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		var current Baseline
		if err := tx.QueryRow(ctx, `SELECT published_sha,publication_generation,publication_hold FROM workspaces WHERE id=$1 FOR UPDATE`,
			s.Workspace).Scan(&current.SHA, &current.Generation, &current.Held); err != nil {
			return err
		}
		if current.Held {
			return ErrHeld
		}
		if current.SHA != receipt.Baseline.SHA || current.Generation != expectedGeneration {
			return ErrStale
		}
		if err := s.lockOperation(ctx, tx, op); err != nil {
			return err
		}
		var staged bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM publication_candidates WHERE workspace_id=$1 AND id=$2
            AND operation_id=$3 AND tree_digest=$4 AND expires_at>clock_timestamp())`,
			s.Workspace, candidateID, op.id, receipt.TreeDigest).Scan(&staged); err != nil {
			return err
		}
		if !staged {
			return ErrStale
		}
		id = uuid.NewString()
		if err := tx.QueryRow(ctx, `INSERT INTO knowledge_releases(workspace_id,id,repository,git_sha,tree_digest,policy_digest,dataset_digest,receipt_id)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id::text`, s.Workspace, id, s.Identity.Repository, receipt.SHA, receipt.TreeDigest,
			receipt.PolicyDigest, receipt.DatasetDigest, candidateID).Scan(&id); err != nil {
			return err
		}
		if err := installIndex(ctx, tx, s.Workspace, candidateID, current.SHA, receipt.SHA); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workspaces SET published_sha=$2,active_release_id=$3,publication_generation=publication_generation+1
            WHERE id=$1`, s.Workspace, receipt.SHA, id); err != nil {
			return err
		}
		return s.finishTx(ctx, tx, op, "promoted", "verified release installed")
	})
	return id, err
}

func installIndex(ctx context.Context, tx pgx.Tx, ws, candidate, base, sha string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM knowledge_items WHERE workspace_id=$1 AND git_sha=$2`, ws, sha); err != nil {
		return err
	}
	if base != "" && base != sha {
		for _, table := range []string{"knowledge_items", "knowledge_search_index"} {
			if _, err := tx.Exec(ctx, `UPDATE `+table+` SET superseded_by=$3 WHERE workspace_id=$1 AND git_sha=$2 AND superseded_by IS NULL`, ws, base, sha); err != nil {
				return err
			}
		}
	}
	var count int64
	for _, table := range []string{"knowledge_items", "knowledge_search_index"} {
		tag, err := tx.Exec(ctx, `UPDATE `+table+` SET git_sha=$3 WHERE workspace_id=$1 AND git_sha=$2`, ws, candidateKey(candidate), sha)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 || (count != 0 && count != tag.RowsAffected()) {
			return ErrStale
		}
		count = tag.RowsAffected()
	}
	return nil
}

func (s *Service) registerCandidate(ctx context.Context, op operation, receipt Receipt) error {
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.lockOperation(ctx, tx, op); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO publication_candidates(workspace_id,id,operation_id,tree_digest)
            VALUES($1,$2,NULLIF($3,'')::uuid,$4)`, s.Workspace, receipt.CandidateID, op.id, receipt.TreeDigest)
		return err
	})
}

func (s *Service) priorReceipt(ctx context.Context, op operation, receipt Receipt) (string, error) {
	var previous string
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.lockOperation(ctx, tx, op); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT r.candidate_id::text FROM publication_receipts r
            JOIN publication_candidates c ON (c.workspace_id,c.id)=(r.workspace_id,r.candidate_id)
            WHERE r.workspace_id=$1 AND r.operation_id=$2 AND r.baseline_sha=$3 AND r.generation=$4
            AND r.git_sha=$5 AND r.tree_digest=$6 AND r.policy_digest=$7 AND r.dataset_digest=$8 AND r.evaluator_digest=$9
            AND r.outcome='passed' AND c.expires_at>clock_timestamp()
            AND EXISTS(SELECT 1 FROM knowledge_items k WHERE k.workspace_id=r.workspace_id AND k.git_sha='candidate:'||r.candidate_id::text)
            ORDER BY r.created_at DESC LIMIT 1`,
			s.Workspace, op.id, receipt.Baseline.SHA, receipt.Generation, receipt.SHA, receipt.TreeDigest,
			receipt.PolicyDigest, receipt.DatasetDigest, receipt.EvaluatorDigest).Scan(&previous)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return previous, err
}
