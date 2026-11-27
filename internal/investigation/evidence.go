package investigation

import (
	"context"
	"errors"
	"strings"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/policy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s Store) Evidence(ctx context.Context, in Input, load func(context.Context) ([]curator.Evidence, error)) ([]curator.Evidence, error) {
	var ready bool
	if err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT evidence_ready FROM investigation_attempts WHERE (workspace_id,id)=($1,$2)`, s.Workspace, in.AttemptID).Scan(&ready)
	}); err != nil {
		return nil, err
	}
	if !ready {
		evidence, err := load(ctx)
		if err != nil {
			return nil, err
		}
		if err = s.writeEvidence(ctx, in, evidence); err != nil {
			return nil, err
		}
	}
	var out []curator.Evidence
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT a.evidence_id,p.location,p.revision,a.digest,p.classification,p.content,p.digest
            FROM attempt_evidence a JOIN source_snapshots p ON (p.workspace_id,p.id)=(a.workspace_id,a.snapshot_id)
            WHERE (a.workspace_id,a.attempt_id)=($1,$2) ORDER BY a.evidence_id`, s.Workspace, in.AttemptID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ev curator.Evidence
			var digest string
			if err := rows.Scan(&ev.ID, &ev.Location, &ev.Revision, &ev.Digest, &ev.Classification, &ev.Content, &digest); err != nil {
				return err
			}
			if digest != ev.Digest || ev.Content == "" {
				return ErrMismatch
			}
			out = append(out, ev)
		}
		return rows.Err()
	})
	return out, err
}

func (s Store) writeEvidence(ctx context.Context, in Input, evidence []curator.Evidence) error {
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		var ready bool
		if err := tx.QueryRow(ctx, `SELECT evidence_ready FROM investigation_attempts WHERE (workspace_id,id)=($1,$2) FOR UPDATE`,
			s.Workspace, in.AttemptID).Scan(&ready); err != nil {
			return err
		}
		if ready {
			return nil
		}
		for _, ev := range evidence {
			if ev.ID == "" || ev.Location == "" || ev.Digest == "" || ev.Revision == "" || ev.Content == "" || len(ev.Content) > 1<<20 ||
				strings.ContainsRune(ev.Content, 0) || policy.Redact(ev.Content) != ev.Content {
				return ErrMismatch
			}
			if _, err := tx.Exec(ctx, `INSERT INTO source_snapshots(id,workspace_id,location,digest,revision,observed_at,classification,content)
                VALUES($1,$2,$3,$4,$5,now(),$6,$7) ON CONFLICT(workspace_id,location,revision) DO UPDATE
                SET content=excluded.content,observed_at=excluded.observed_at
                WHERE source_snapshots.content='' AND source_snapshots.digest=excluded.digest AND source_snapshots.classification=excluded.classification`,
				uuid.NewString(), s.Workspace, ev.Location, ev.Digest, ev.Revision, ev.Classification, ev.Content); err != nil {
				return err
			}
			var id string
			err := tx.QueryRow(ctx, `SELECT id::text FROM source_snapshots WHERE workspace_id=$1 AND location=$2 AND revision=$3
                AND digest=$4 AND classification=$5 AND content=$6`, s.Workspace, ev.Location, ev.Revision, ev.Digest, ev.Classification, ev.Content).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrMismatch
			}
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO attempt_evidence(workspace_id,attempt_id,evidence_id,snapshot_id,digest)
                VALUES($1,$2,$3,$4,$5)`, s.Workspace, in.AttemptID, ev.ID, id, ev.Digest); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE investigation_attempts SET evidence_ready=true WHERE (workspace_id,id)=($1,$2)`, s.Workspace, in.AttemptID)
		return err
	})
}

func (s Store) Feedback(ctx context.Context, in Input) ([]string, error) {
	var out []string
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		var ready bool
		if err := tx.QueryRow(ctx, `SELECT feedback_ready FROM investigation_attempts WHERE (workspace_id,id)=($1,$2)`, s.Workspace, in.AttemptID).Scan(&ready); err != nil {
			return err
		}
		if !ready {
			if _, err := tx.Exec(ctx, `INSERT INTO attempt_feedback(workspace_id,attempt_id,feedback_id)
            SELECT f.workspace_id,$2,f.id FROM feedback f
            JOIN gap_reports g ON g.workspace_id=f.workspace_id AND g.id=$3
            JOIN knowledge_releases r ON r.workspace_id=f.workspace_id AND r.id=$4
            WHERE f.workspace_id=$1 AND f.revision=r.git_sha AND f.knowledge_uri=ANY(g.knowledge_refs) AND f.consumed_at IS NULL
            ORDER BY f.created_at,f.id LIMIT 50 ON CONFLICT DO NOTHING`, s.Workspace, in.AttemptID, in.GapID, in.BaseReleaseID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE investigation_attempts SET feedback_ready=true WHERE (workspace_id,id)=($1,$2)`,
				s.Workspace, in.AttemptID); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `SELECT f.observation FROM feedback f JOIN attempt_feedback a ON (a.workspace_id,a.feedback_id)=(f.workspace_id,f.id)
            WHERE (a.workspace_id,a.attempt_id)=($1,$2) ORDER BY f.created_at,f.id`, s.Workspace, in.AttemptID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var body string
			if err := rows.Scan(&body); err != nil {
				return err
			}
			out = append(out, body)
		}
		return rows.Err()
	})
	return out, err
}
