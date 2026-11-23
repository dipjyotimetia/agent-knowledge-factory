// Package audit appends hash-chained records. Each row commits to the previous
// row in the same workspace, so a deleted or rewritten event breaks the chain.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Event is one audit record. Details must already be redacted.
type Event struct {
	Actor      string
	RequestID  string
	Action     string
	ObjectType string
	ObjectID   string
	Revision   string
	Details    any
}

// Append inserts one event and links it to the previous hash.
func Append(ctx context.Context, tx pgx.Tx, workspaceID string, event Event) (string, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "audit:"+workspaceID); err != nil {
		return "", err
	}
	var prev string
	var seq int64
	err := tx.QueryRow(ctx, `
		SELECT event_hash, chain_seq FROM audit_events
		WHERE workspace_id = $1
		ORDER BY chain_seq DESC, created_at DESC
		LIMIT 1
	`, workspaceID).Scan(&prev, &seq)
	if errors.Is(err, pgx.ErrNoRows) {
		prev = ""
	} else if err != nil {
		return "", err
	}
	details, err := canonicalJSON(event.Details)
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	if event.RequestID == "" {
		event.RequestID = id
	}
	sum := sha256.Sum256([]byte(hashMaterial(prev, id, event.Actor, event.Action, event.ObjectType, event.ObjectID, event.Revision, details)))
	hash := hex.EncodeToString(sum[:])
	_, err = tx.Exec(ctx, `
		INSERT INTO audit_events (
			id, workspace_id, actor, request_id, action, object_type, object_id, object_revision, details, prev_hash, event_hash, chain_seq
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
	`, id, workspaceID, event.Actor, event.RequestID, event.Action, event.ObjectType, event.ObjectID, event.Revision, details, prev, hash, seq+1)
	if err != nil {
		return "", err
	}
	return hash, nil
}

// Verify reports whether every non-empty hash matches the previous row.
func Verify(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	rows, err := tx.Query(ctx, `
		SELECT id, actor, action, object_type, object_id, object_revision, details, prev_hash, event_hash
		FROM audit_events
		WHERE workspace_id = $1 AND event_hash <> ''
		ORDER BY chain_seq, id
	`, workspaceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	prev := ""
	for rows.Next() {
		var id, actor, action, objectType, objectID, revision, prior, hash string
		var details []byte
		if err := rows.Scan(&id, &actor, &action, &objectType, &objectID, &revision, &details, &prior, &hash); err != nil {
			return err
		}
		if prior != prev {
			return ErrBrokenChain
		}
		canon, err := canonicalJSON(json.RawMessage(details))
		if err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(hashMaterial(prev, id, actor, action, objectType, objectID, revision, canon)))
		if hex.EncodeToString(sum[:]) != hash {
			return ErrBrokenChain
		}
		prev = hash
	}
	return rows.Err()
}

// ErrBroken is a mismatched audit hash.
var errBroken = brokenError{}

type brokenError struct{}

func (brokenError) Error() string { return "audit hash chain is broken" }

func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}

func hashMaterial(prev, id, actor, action, objectType, objectID, revision string, details []byte) string {
	return prev + "\n" + id + "\n" + actor + "\n" + action + "\n" + objectType + "\n" + objectID + "\n" + revision + "\n" + string(details)
}

// ErrBrokenChain is returned when Verify fails.
var ErrBrokenChain = errBroken
