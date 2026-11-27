package index

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PublishedReader is the retrieval capability for agent-facing surfaces.
type PublishedReader struct{ service *Service }

func (s *Service) Published() PublishedReader { return PublishedReader{service: s} }

func (r PublishedReader) Search(ctx context.Context, workspace, query, itemType string, limit int) ([]Hit, error) {
	return r.SearchRevision(ctx, workspace, query, itemType, limit, "")
}

func (r PublishedReader) SearchRevision(ctx context.Context, workspace, query, itemType string, limit int, revision string) ([]Hit, error) {
	return r.service.queryHits(ctx, readScope{workspace: workspace, kind: "published", revision: revision, fixture: r.service.fixture}, query, itemType, limit, true)
}

func (r PublishedReader) Get(ctx context.Context, workspace, uri, revision string) (Item, error) {
	return r.service.get(ctx, readScope{workspace: workspace, kind: "published", revision: revision, fixture: r.service.fixture}, uri)
}

// Search and Get on an index service also enforce the published boundary.
func (s *Service) Search(ctx context.Context, workspace, query, itemType string, limit int) ([]Hit, error) {
	return s.Published().Search(ctx, workspace, query, itemType, limit)
}

func (s *Service) Get(ctx context.Context, workspace, uri, revision string) (Item, error) {
	return s.Published().Get(ctx, workspace, uri, revision)
}

// EvaluationReader is bound to one private candidate or one pinned release.
// The caller cannot supply a workspace, candidate, or revision when reading.
type EvaluationReader struct {
	service *Service
	scope   readScope
}

func (r EvaluationReader) Search(ctx context.Context, query string, limit int) ([]Hit, error) {
	return r.service.queryHits(ctx, r.scope, query, "", limit, false)
}

func (r EvaluationReader) Get(ctx context.Context, uri string) (Item, error) {
	return r.service.get(ctx, r.scope, uri)
}

// BaselineForEvaluation pins both the immutable release ID and its SHA.
func (s *Service) BaselineForEvaluation(workspace, releaseID, sha string) EvaluationReader {
	return EvaluationReader{service: s, scope: readScope{workspace: workspace, kind: "baseline", revision: sha, id: releaseID}}
}

type readScope struct {
	workspace, kind, revision, id, operation string
	fixture                                  bool
}

func (s readScope) args() []any {
	return []any{s.workspace, s.kind, s.revision, s.id, s.operation, s.fixture}
}

// Keep the pointer and release check in the statement that reads the items.
const readScopeSQL = `
	WITH scope AS (
		SELECT CASE $2::text
			WHEN 'published' THEN CASE WHEN w.published_sha <> ''
				AND w.published_sha NOT LIKE 'candidate:%'
				AND ($3::text = '' OR $3 = w.published_sha)
				AND ($6::boolean OR EXISTS (
					SELECT 1 FROM knowledge_releases r WHERE (r.workspace_id,r.id,r.git_sha)=(w.id,w.active_release_id,w.published_sha)
				)) THEN w.published_sha END
			WHEN 'baseline' THEN (
				SELECT r.git_sha FROM knowledge_releases r
				WHERE r.workspace_id=w.id AND r.id::text=$4 AND r.git_sha=$3
			)
			WHEN 'candidate' THEN (
				SELECT 'candidate:'||c.id::text FROM publication_candidates c
				WHERE c.workspace_id=w.id AND c.id::text=$4
				  AND c.operation_id::text IS NOT DISTINCT FROM NULLIF($5::text,'')
				  AND c.expires_at>clock_timestamp()
			)
			WHEN 'fixture' THEN CASE WHEN $6 THEN $3 END
		END AS sha
		FROM workspaces w WHERE w.id=$1
	)
`

func candidateRevision(id string) (string, error) {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.String() != id {
		return "", ErrNotFound
	}
	return "candidate:" + id, nil
}

// StageCandidate creates the private corpus once and returns its evaluation capability.
func (s *Service) StageCandidate(ctx context.Context, workspace, id, operation, root string) (EvaluationReader, error) {
	key, err := candidateRevision(id)
	if err != nil {
		return EvaluationReader{}, err
	}
	docs, err := s.loadDocs(root)
	if err != nil {
		return EvaluationReader{}, err
	}
	scope := readScope{workspace: workspace, kind: "candidate", id: id, operation: operation}
	err = s.DB.WithWorkspace(ctx, workspace, func(ctx context.Context, tx pgx.Tx) error {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR NO KEY UPDATE`, workspace).Scan(&locked); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var staged *string
		if err := tx.QueryRow(ctx, readScopeSQL+`SELECT sha FROM scope
			WHERE NOT EXISTS (SELECT 1 FROM knowledge_items k WHERE k.workspace_id=$1 AND k.git_sha=scope.sha)
			  AND NOT EXISTS (SELECT 1 FROM publication_receipts r WHERE r.workspace_id=$1 AND r.candidate_id::text=$4)`,
			scope.args()...).Scan(&staged); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if staged == nil || *staged != key {
			return ErrNotFound
		}
		return insertDocs(ctx, tx, workspace, key, docs)
	})
	if err != nil {
		return EvaluationReader{}, err
	}
	return EvaluationReader{service: s, scope: scope}, nil
}

func (s *Service) DropCandidate(ctx context.Context, workspace, id string) error {
	key, err := candidateRevision(id)
	if err != nil {
		return err
	}
	return s.dropStage(ctx, workspace, key)
}

func (s *Service) FixtureForEvaluation(workspace, revision string) EvaluationReader {
	return EvaluationReader{service: s, scope: readScope{workspace: workspace, kind: "fixture", revision: revision,
		fixture: s.fixture && !strings.HasPrefix(revision, "candidate:")}}
}
