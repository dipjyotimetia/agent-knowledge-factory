package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/policy"
	"github.com/jackc/pgx/v5"
)

// EnforceRetention deletes sanitized operational rows older than the workspace
// retention window. Audit events are kept. Snapshot bodies are included in the
// deletion; they are never copied into Git by this function.
func (db *DB) EnforceRetention(ctx context.Context, workspaceID string) error {
	return db.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `WITH expired AS (
            DELETE FROM publication_candidates c WHERE c.workspace_id=$1 AND c.expires_at<clock_timestamp()
            AND NOT EXISTS (SELECT 1 FROM publication_operations o WHERE (o.workspace_id,o.id)=(c.workspace_id,c.operation_id)
              AND o.lease_until>clock_timestamp()) RETURNING id
        ) DELETE FROM knowledge_items k WHERE k.workspace_id=$1
          AND k.git_sha IN (SELECT 'candidate:'||id::text FROM expired)`, workspaceID); err != nil {
			return err
		}
		var days int
		if err := tx.QueryRow(ctx, `SELECT retention_days FROM workspaces WHERE id = $1`, workspaceID).Scan(&days); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM query_logs
			WHERE workspace_id = $1 AND created_at < now() - make_interval(days => $2)
		`, workspaceID, days); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM evidence_links
			WHERE snapshot_id IN (
				SELECT id FROM source_snapshots
				WHERE workspace_id = $1 AND observed_at < now() - make_interval(days => $2)
			)
		`, workspaceID, days); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			DELETE FROM source_snapshots
			WHERE workspace_id = $1 AND observed_at < now() - make_interval(days => $2)
		`, workspaceID, days)
		return err
	})
}

// Export returns gap and proposal metadata. Snapshot bodies are omitted.
// The payload is rejected if a secret pattern is still present.
func (db *DB) Export(ctx context.Context, workspaceID string) ([]byte, error) {
	var payload []byte
	err := db.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id::text, state, category, summary FROM gap_reports ORDER BY created_at
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		var gaps []map[string]string
		for rows.Next() {
			var id, state, category, summary string
			if err := rows.Scan(&id, &state, &category, &summary); err != nil {
				return err
			}
			gaps = append(gaps, map[string]string{"id": id, "state": state, "category": category, "summary": summary})
		}
		if err := rows.Err(); err != nil {
			return err
		}
		raw, err := json.Marshal(map[string]any{"gaps": gaps})
		if err != nil {
			return err
		}
		if policy.Redact(string(raw)) == "[redacted]" {
			return fmt.Errorf("export contained a secret and was refused")
		}
		payload = raw
		return nil
	})
	return payload, err
}
