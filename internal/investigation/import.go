package investigation

import (
	"context"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/audit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type LegacyProposal struct {
	ProposalID, GapID string
}

func (s Store) LegacyPage(ctx context.Context, after string) ([]LegacyProposal, error) {
	var out []LegacyProposal
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT p.id::text,g.id::text FROM proposals p
            JOIN gap_reports g ON g.workspace_id=p.workspace_id AND g.id=p.gap_ids[1]
            WHERE p.workspace_id=$1 AND p.id>coalesce(NULLIF($2,'')::uuid,'00000000-0000-0000-0000-000000000000')
            AND cardinality(p.gap_ids)=1 AND g.canonical_gap_id IS NULL AND g.state IN ('awaiting-review','accepted')
            AND NOT EXISTS(SELECT 1 FROM investigation_attempts a WHERE (a.workspace_id,a.gap_id)=(g.workspace_id,g.id))
            ORDER BY p.id LIMIT 50`, s.Workspace, after)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p LegacyProposal
			if err := rows.Scan(&p.ProposalID, &p.GapID); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

func (s Store) Import(ctx context.Context, p LegacyProposal, valid bool) error {
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.lockQueue(ctx, tx); err != nil {
			return err
		}
		var matches bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM proposals WHERE workspace_id=$1 AND id=$2 AND gap_ids=ARRAY[$3]::uuid[])`,
			s.Workspace, p.ProposalID, p.GapID).Scan(&matches); err != nil {
			return err
		}
		if !matches {
			return ErrMismatch
		}
		state, result := "parked", "ambiguous-legacy-mapping"
		if valid {
			state, result = "queued", "verified-legacy-mapping"
		}
		id := uuid.NewString()
		tag, err := tx.Exec(ctx, `INSERT INTO investigation_attempts(workspace_id,id,gap_id,policy_version,state,proposal_id,legacy_review,result)
            VALUES($1,$2,$3,$4,$5,$6,true,$7) ON CONFLICT DO NOTHING`, s.Workspace, id, p.GapID, s.PolicyVersion, state, p.ProposalID, result)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = audit.Append(ctx, tx, s.Workspace, audit.Event{Actor: "operator", Action: "legacy-review-imported", ObjectType: "attempt", ObjectID: id,
			Details: map[string]string{"result": result}})
		return err
	})
}
