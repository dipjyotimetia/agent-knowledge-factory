// Package index builds the rebuildable Postgres full-text index from a Git tree
// of OKF concepts and Agent Skills. Search never returns a SHA that is not the
// workspace's published revision, and it never returns draft, deprecated, or
// superseded documents.
package index

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/audit"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/knowledge/okf"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/knowledge/skills"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/policy"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrInvalidQuery means the caller sent an empty or oversized search.
var ErrInvalidQuery = errors.New("invalid search query")

// ErrNotFound means the URI is not in the published revision.
var ErrNotFound = errors.New("knowledge item not found")

// Service indexes one workspace at a time.
type Service struct {
	DB  *postgres.DB
	Now func() time.Time
}

// New returns a search service.
func New(db *postgres.DB) *Service {
	return &Service{DB: db, Now: time.Now}
}

// Document is one indexed item.
type Document struct {
	URI          string
	ItemType     string
	ConceptType  string
	Title        string
	Description  string
	Body         string
	Path         string
	Status       string
	TrustTier    string
	Approved     bool
	Superseded   string
	Stale        bool
	Warnings     []string
	Headings     string
	Metadata     map[string]any
	LastReviewed time.Time
}

// Hit is a search result. Snippet is a short extract, not the whole document.
type Hit struct {
	URI          string   `json:"uri"`
	Title        string   `json:"title"`
	Snippet      string   `json:"snippet"`
	Revision     string   `json:"revision"`
	Path         string   `json:"path"`
	ItemType     string   `json:"type"`
	TrustTier    string   `json:"trust_tier"`
	Stale        bool     `json:"stale"`
	Warnings     []string `json:"warnings"`
	Sources      []string `json:"sources"`
	Description  string   `json:"description"`
	LastReviewed string   `json:"last_reviewed,omitempty"`
}

// Item is a full published document.
type Item struct {
	Hit
	Body string `json:"body"`
}

// Reindex loads root, writes items for sha, and points the workspace at that SHA
// in the same transaction. The previous SHA's rows stay so a rollback can
// republish them.
func (s *Service) Reindex(ctx context.Context, workspaceID, sha, root string) error {
	if strings.TrimSpace(sha) == "" {
		return errors.New("sha is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	now := s.now()
	bundle, err := okf.Load(abs, okf.Options{Now: now})
	if err != nil {
		return err
	}
	if !bundle.Conformant() {
		return fmt.Errorf("knowledge bundle is not conformant")
	}
	loaded, err := skills.LoadTree(abs)
	if err != nil {
		return err
	}
	for _, skill := range loaded {
		if !skill.Valid() {
			return fmt.Errorf("skill %s is not valid", skill.Name)
		}
	}
	docs := documents(bundle, loaded, s.now())
	return s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		return replaceSHA(ctx, tx, workspaceID, sha, docs)
	})
}

// Stage writes a candidate revision without changing the published SHA.
func (s *Service) Stage(ctx context.Context, workspaceID, sha, root string) error {
	docs, err := s.loadDocs(root)
	if err != nil {
		return err
	}
	return s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		return insertDocs(ctx, tx, workspaceID, sha, docs)
	})
}

// Promote makes a staged revision the published SHA and marks the previous one superseded.
func (s *Service) Promote(ctx context.Context, workspaceID, sha string) error {
	return s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM knowledge_items WHERE workspace_id = $1 AND git_sha = $2`, workspaceID, sha).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("no staged revision %s", sha)
		}
		var previous string
		if err := tx.QueryRow(ctx, `SELECT published_sha FROM workspaces WHERE id = $1`, workspaceID).Scan(&previous); err != nil {
			return err
		}
		if previous != "" && previous != sha {
			if _, err := tx.Exec(ctx, `
				UPDATE knowledge_items SET superseded_by = $1
				WHERE workspace_id = $2 AND git_sha = $3 AND coalesce(superseded_by, '') = ''
			`, sha, workspaceID, previous); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE knowledge_search_index SET superseded_by = $1
				WHERE workspace_id = $2 AND git_sha = $3 AND coalesce(superseded_by, '') = ''
			`, sha, workspaceID, previous); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE workspaces SET published_sha = $1 WHERE id = $2`, sha, workspaceID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return postgres.ErrNotFound
		}
		return nil
	})
}

// DropStage deletes a revision that was not promoted.
func (s *Service) DropStage(ctx context.Context, workspaceID, sha string) error {
	return s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		var published string
		if err := tx.QueryRow(ctx, `SELECT published_sha FROM workspaces WHERE id = $1`, workspaceID).Scan(&published); err != nil {
			return err
		}
		if published == sha {
			return nil
		}
		if _, err := tx.Exec(ctx, `DELETE FROM knowledge_search_index WHERE workspace_id = $1 AND git_sha = $2`, workspaceID, sha); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM knowledge_items WHERE workspace_id = $1 AND git_sha = $2`, workspaceID, sha)
		return err
	})
}

func (s *Service) loadDocs(root string) ([]Document, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	now := s.now()
	bundle, err := okf.Load(abs, okf.Options{Now: now})
	if err != nil {
		return nil, err
	}
	if !bundle.Conformant() {
		return nil, fmt.Errorf("knowledge bundle is not conformant")
	}
	loaded, err := skills.LoadTree(abs)
	if err != nil {
		return nil, err
	}
	for _, skill := range loaded {
		if !skill.Valid() {
			return nil, fmt.Errorf("skill %s is not valid", skill.Name)
		}
	}
	return documents(bundle, loaded, s.now()), nil
}

// Rollback points the workspace at a revision that is already indexed and
// clears the superseded mark on that revision so search can return it again.
// It records an audit event. It does not delete the newer rows.
func (s *Service) Rollback(ctx context.Context, workspaceID, sha, actor string) error {
	if strings.TrimSpace(sha) == "" {
		return errors.New("sha is required")
	}
	if actor == "" {
		actor = "cli"
	}
	return s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM knowledge_items WHERE workspace_id = $1 AND git_sha = $2
		`, workspaceID, sha).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("no indexed revision %s", sha)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_items SET superseded_by = NULL
			WHERE workspace_id = $1 AND git_sha = $2
		`, workspaceID, sha); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_search_index SET superseded_by = NULL
			WHERE workspace_id = $1 AND git_sha = $2
		`, workspaceID, sha); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE workspaces SET published_sha = $1 WHERE id = $2`, sha, workspaceID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return postgres.ErrNotFound
		}
		_, err = audit.Append(ctx, tx, workspaceID, audit.Event{
			Actor:      actor,
			Action:     "rollback",
			ObjectType: "workspace",
			ObjectID:   workspaceID,
			Revision:   sha,
		})
		return err
	})
}

// SyncDelivery reindexes sha once per webhook delivery id. A repeated delivery
// is a no-op and does not move the published SHA again.
func (s *Service) SyncDelivery(ctx context.Context, workspaceID, deliveryID, sha, root string) (bool, error) {
	if strings.TrimSpace(deliveryID) == "" {
		return false, errors.New("delivery id is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	bundle, err := okf.Load(abs, okf.Options{Now: s.now()})
	if err != nil {
		return false, err
	}
	if !bundle.Conformant() {
		return false, fmt.Errorf("knowledge bundle is not conformant")
	}
	loaded, err := skills.LoadTree(abs)
	if err != nil {
		return false, err
	}
	for _, skill := range loaded {
		if !skill.Valid() {
			return false, fmt.Errorf("skill %s is not valid", skill.Name)
		}
	}
	docs := documents(bundle, loaded, s.now())
	applied := false
	err = s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO webhook_deliveries (workspace_id, delivery_id)
			VALUES ($1, $2)
			ON CONFLICT DO NOTHING
		`, workspaceID, deliveryID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		applied = true
		return replaceSHA(ctx, tx, workspaceID, sha, docs)
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

func replaceSHA(ctx context.Context, tx pgx.Tx, workspaceID, sha string, docs []Document) error {
	var previous string
	if err := tx.QueryRow(ctx, `SELECT published_sha FROM workspaces WHERE id = $1`, workspaceID).Scan(&previous); err != nil {
		return err
	}
	if previous != "" && previous != sha {
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_items SET superseded_by = $1
			WHERE workspace_id = $2 AND git_sha = $3 AND coalesce(superseded_by, '') = ''
		`, sha, workspaceID, previous); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_search_index SET superseded_by = $1
			WHERE workspace_id = $2 AND git_sha = $3 AND coalesce(superseded_by, '') = ''
		`, sha, workspaceID, previous); err != nil {
			return err
		}
	}
	if err := insertDocs(ctx, tx, workspaceID, sha, docs); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE workspaces SET published_sha = $1 WHERE id = $2`, sha, workspaceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return postgres.ErrNotFound
	}
	return nil
}

func insertDocs(ctx context.Context, tx pgx.Tx, workspaceID, sha string, docs []Document) error {
	if _, err := tx.Exec(ctx, `DELETE FROM knowledge_items WHERE git_sha = $1 AND workspace_id = $2`, sha, workspaceID); err != nil {
		return err
	}
	for _, doc := range docs {
		id := uuid.New()
		meta, err := json.Marshal(doc.Metadata)
		if err != nil {
			return err
		}
		warnings, err := json.Marshal(doc.Warnings)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
				INSERT INTO knowledge_items (
					id, workspace_id, uri, item_type, concept_type, title, description, body,
					git_sha, path, status, trust_tier, metadata, approved, superseded_by,
					stale, warnings, headings, last_reviewed_at
				) VALUES (
					$1, $2, $3, $4, $5, $6, $7, $8,
					$9, $10, $11, $12, $13, $14, NULLIF($15, ''),
					$16, $17, $18, $19
				)
			`, id, workspaceID, doc.URI, doc.ItemType, doc.ConceptType, doc.Title, doc.Description, doc.Body,
			sha, doc.Path, doc.Status, doc.TrustTier, meta, doc.Approved, doc.Superseded,
			doc.Stale, warnings, doc.Headings, doc.LastReviewed); err != nil {
			return fmt.Errorf("insert %s: %w", doc.URI, err)
		}
		if _, err := tx.Exec(ctx, `
				INSERT INTO knowledge_search_index (
					knowledge_item_id, workspace_id, document, approved, git_sha, status, superseded_by
				)
				SELECT id, workspace_id, search_vector, approved, git_sha, status, superseded_by
				FROM knowledge_items WHERE id = $1
			`, id); err != nil {
			return fmt.Errorf("search index %s: %w", doc.URI, err)
		}
	}
	return nil
}

// Search returns published, approved, stable documents in this workspace.
func (s *Service) Search(ctx context.Context, workspaceID, query, itemType string, limit int) ([]Hit, error) {
	raw := strings.TrimSpace(query)
	if raw == "" || utf8.RuneCountInString(raw) > 200 {
		return nil, ErrInvalidQuery
	}
	logged := policy.Redact(raw)
	if logged == "[redacted]" {
		if err := s.logQuery(ctx, workspaceID, logged, 0); err != nil {
			return nil, err
		}
		return []Hit{}, nil
	}
	return s.queryHits(ctx, workspaceID, query, itemType, limit, "", true)
}

// SearchAt scores one staged or historical revision. It does not require that SHA to be published.
func (s *Service) SearchAt(ctx context.Context, workspaceID, sha, query, itemType string, limit int) ([]Hit, error) {
	if strings.TrimSpace(sha) == "" {
		return nil, errors.New("sha is required")
	}
	return s.queryHits(ctx, workspaceID, query, itemType, limit, sha, false)
}

func (s *Service) queryHits(ctx context.Context, workspaceID, rawQuery, itemType string, limit int, sha string, log bool) ([]Hit, error) {
	raw := strings.TrimSpace(rawQuery)
	if raw == "" || utf8.RuneCountInString(raw) > 200 {
		return nil, ErrInvalidQuery
	}
	if log {
		logged := policy.Redact(raw)
		if logged == "[redacted]" {
			if err := s.logQuery(ctx, workspaceID, logged, 0); err != nil {
				return nil, err
			}
			return []Hit{}, nil
		}
	}
	query := expandQuery(raw)
	idents := queryIdents(raw)
	if idents == nil {
		idents = []string{}
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 20 {
		limit = 20
	}
	var hits []Hit
	err := s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT ki.uri, ki.title, ki.description, left(ki.body, 280), ki.git_sha, ki.path,
			       ki.item_type, ki.trust_tier, ki.stale, ki.warnings, ki.metadata, ki.last_reviewed_at
			FROM knowledge_search_index si
			JOIN knowledge_items ki ON ki.id = si.knowledge_item_id
			JOIN workspaces w ON w.id = si.workspace_id
			WHERE si.workspace_id = $1
			  AND si.approved = true
			  AND si.status = 'stable'
			  AND coalesce(si.superseded_by, '') = ''
			  AND ($2 = '' OR ki.item_type = $2)
			  AND (
			    ($5 = '' AND w.published_sha = si.git_sha)
			    OR ($5 <> '' AND si.git_sha = $5)
			  )
			  AND (
			    si.document @@ websearch_to_tsquery('english', $3)
			    OR similarity(ki.title, $3) > 0.2
			    OR EXISTS (
			      SELECT 1 FROM unnest($6::text[]) AS ident
			      WHERE ident <> '' AND position(lower(ident) in lower(ki.body)) > 0
			    )
			  )
			ORDER BY ts_rank(si.document, websearch_to_tsquery('english', $3)) DESC,
			         similarity(ki.title, $3) DESC,
			         ki.uri
			LIMIT $4
		`, workspaceID, itemType, query, limit, sha, idents)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var hit Hit
			var warnings, meta []byte
			var reviewed *time.Time
			if err := rows.Scan(&hit.URI, &hit.Title, &hit.Description, &hit.Snippet, &hit.Revision, &hit.Path,
				&hit.ItemType, &hit.TrustTier, &hit.Stale, &warnings, &meta, &reviewed); err != nil {
				return err
			}
			if reviewed != nil {
				hit.LastReviewed = reviewed.UTC().Format(time.RFC3339)
			}
			if err := json.Unmarshal(warnings, &hit.Warnings); err != nil {
				return err
			}
			hit.Sources = sourceRefs(meta)
			if hit.Stale {
				hit.Warnings = appendUnique(hit.Warnings, "stale")
			}
			hits = append(hits, hit)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if hits == nil {
		hits = []Hit{}
	}
	if log {
		if err := s.logQuery(ctx, workspaceID, policy.Redact(raw), len(hits)); err != nil {
			return nil, err
		}
	}
	return hits, nil
}

func (s *Service) logQuery(ctx context.Context, workspaceID, redacted string, count int) error {
	return s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO query_logs (id, workspace_id, query_redacted, result_count)
			VALUES ($1, $2, $3, $4)
		`, uuid.New(), workspaceID, redacted, count)
		return err
	})
}

// Get returns one published item. revision empty means the published SHA.
func (s *Service) Get(ctx context.Context, workspaceID, uri, revision string) (Item, error) {
	var item Item
	err := s.DB.WithWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT ki.uri, ki.title, ki.description, ki.body, ki.git_sha, ki.path,
			       ki.item_type, ki.trust_tier, ki.stale, ki.warnings, ki.metadata, ki.last_reviewed_at
			FROM knowledge_items ki
			JOIN workspaces w ON w.id = ki.workspace_id
			WHERE ki.uri = $1
			  AND ki.approved = true
			  AND ki.status = 'stable'
			  AND coalesce(ki.superseded_by, '') = ''
			  AND ki.git_sha = CASE WHEN $2 = '' THEN w.published_sha ELSE $2 END
			  AND ($2 <> '' OR ki.git_sha = w.published_sha)
		`, uri, revision)
		var warnings, meta []byte
		var reviewed *time.Time
		if err := row.Scan(&item.URI, &item.Title, &item.Description, &item.Body, &item.Revision, &item.Path,
			&item.ItemType, &item.TrustTier, &item.Stale, &warnings, &meta, &reviewed); err != nil {
			return err
		}
		if reviewed != nil {
			item.LastReviewed = reviewed.UTC().Format(time.RFC3339)
		}
		if err := json.Unmarshal(warnings, &item.Warnings); err != nil {
			return err
		}
		item.Sources = sourceRefs(meta)
		item.Snippet = clip(item.Body, 280)
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, err
	}
	return item, nil
}

func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now()
}

func documents(bundle *okf.Bundle, loaded []*skills.Skill, now time.Time) []Document {
	prefix := bundle.URIPrefix
	if prefix == "" {
		prefix = "kb://bundle"
	}
	var docs []Document
	for _, concept := range bundle.Concepts {
		warnings := []string{}
		if concept.Stale || concept.StaleMalformed {
			warnings = append(warnings, "stale")
		}
		if concept.StatusUnknown {
			warnings = append(warnings, "unknown-status")
		}
		approved := concept.Status == "stable" && concept.SupersededBy == "" && !concept.StatusUnknown
		meta := map[string]any{"extensions": concept.Extensions}
		var sources []string
		for _, src := range concept.Sources {
			if src.Ignored || src.Resource == "" {
				continue
			}
			sources = append(sources, src.Resource)
		}
		meta["sources"] = sources
		docs = append(docs, Document{
			URI:          prefix + "/" + concept.ID,
			ItemType:     "concept",
			ConceptType:  concept.Type,
			Title:        concept.Title,
			Description:  concept.Description,
			Body:         concept.Body,
			Path:         concept.Path,
			Status:       concept.Status,
			TrustTier:    concept.TrustTier,
			Approved:     approved,
			Superseded:   concept.SupersededBy,
			Stale:        concept.Stale || concept.StaleMalformed,
			Warnings:     warnings,
			Headings:     strings.Join(concept.Headings, "\n"),
			Metadata:     meta,
			LastReviewed: reviewedAt(concept, now),
		})
	}
	for _, skill := range loaded {
		rel, err := filepath.Rel(bundle.Root, skill.Dir)
		if err != nil {
			rel = skill.Name
		}
		rel = filepath.ToSlash(rel)
		docs = append(docs, Document{
			URI:          prefix + "/" + rel,
			ItemType:     "skill",
			ConceptType:  "skill",
			Title:        skill.Name,
			Description:  skill.Description,
			Body:         skill.Body,
			Path:         rel + "/SKILL.md",
			Status:       "stable",
			TrustTier:    "unverified",
			Approved:     true,
			Headings:     "Procedure",
			Metadata:     map[string]any{"license": skill.License, "metadata": skill.Metadata},
			LastReviewed: now.UTC(),
		})
	}
	return docs
}

func reviewedAt(concept okf.Concept, fallback time.Time) time.Time {
	var latest time.Time
	for _, event := range concept.Verified {
		if event.At.Valid && event.At.Time.After(latest) {
			latest = event.At.Time
		}
	}
	if latest.IsZero() {
		return fallback.UTC()
	}
	return latest.UTC()
}

func sourceRefs(meta []byte) []string {
	var decoded map[string]any
	if err := json.Unmarshal(meta, &decoded); err != nil {
		return nil
	}
	raw, _ := decoded["sources"].([]any)
	var out []string
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

var identPattern = regexp.MustCompile(`[A-Za-z][a-z0-9]+[A-Z][A-Za-z0-9]*|[A-Za-z0-9]+(?:-[A-Za-z0-9]+){1,}`)

func queryIdents(query string) []string {
	return identPattern.FindAllString(query, -1)
}

func expandQuery(query string) string {
	lower := strings.ToLower(query)
	extra := ""
	if strings.Contains(lower, "dedupe") || strings.Contains(lower, "dedup") {
		extra = " deduplicate deduplication"
	}
	return query + extra
}

func appendUnique(list []string, value string) []string {
	for _, item := range list {
		if item == value {
			return list
		}
	}
	return append(list, value)
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n])
}
