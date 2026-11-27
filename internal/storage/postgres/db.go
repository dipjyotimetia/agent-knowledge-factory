// Package postgres stores operational state. Approved knowledge content stays
// in Git; this package keeps the rebuildable index and the gap history.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a workspace-scoped read matches nothing.
// Row-level security makes another tenant's row indistinguishable from a
// missing row.
var ErrNotFound = errors.New("not found")

// DB is a connection pool. Installer is true only for the migration and
// bootstrap connection. The HTTP server must be given Installer=false so a
// request handler cannot create tenants or skip row-level security.
type DB struct {
	pool      *pgxpool.Pool
	Installer bool
}

// Open connects to Postgres.
func Open(ctx context.Context, databaseURL string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Close releases the pool.
func (db *DB) Close() {
	if db != nil && db.pool != nil {
		db.pool.Close()
	}
}

// Ping reports readiness.
func (db *DB) Ping(ctx context.Context) error {
	return db.pool.Ping(ctx)
}

// WithWorkspace runs fn inside a transaction whose row-level security setting
// is the authenticated workspace. The setting is local to the transaction.
func (db *DB) WithWorkspace(ctx context.Context, workspaceID string, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('akf.workspace_id', $1, true)", workspaceID); err != nil {
		return fmt.Errorf("set workspace: %w", err)
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Migrate applies embedded SQL files in order. It must run with a role that
// can create extensions and tables. That role is not the API role.
// Files are executed with the simple query protocol so a migration can
// contain more than one statement, including DO blocks.
func (db *DB) Migrate(ctx context.Context, files fs.FS) error {
	if !db.Installer {
		return errors.New("migrations require the installer database connection")
	}
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()
	if err := execSimple(ctx, conn.Conn(), `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("schema_migrations: %w", err)
	}
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		name := entry.Name()
		if strings.ContainsAny(name, "'\\") {
			return fmt.Errorf("migration name %q is not safe to record", name)
		}
		var applied bool
		if err := db.pool.QueryRow(ctx, "SELECT true FROM schema_migrations WHERE version = $1", name).Scan(&applied); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("migration status %s: %w", name, err)
		}
		if applied {
			continue
		}
		body, err := fs.ReadFile(files, name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		script := "BEGIN;\n" + string(body) + "\nINSERT INTO schema_migrations (version) VALUES ('" + name + "');\nCOMMIT;\n"
		if err := execSimple(ctx, conn.Conn(), script); err != nil {
			_ = execSimple(ctx, conn.Conn(), "ROLLBACK")
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}

func execSimple(ctx context.Context, conn *pgx.Conn, sql string) error {
	result := conn.PgConn().Exec(ctx, sql)
	if _, err := result.ReadAll(); err != nil {
		return err
	}
	return result.Close()
}

// GrantAppRole lets the non-superuser API role use the migrated tables.
// schema_migrations stays installer-only.
func (db *DB) GrantAppRole(ctx context.Context, role string) error {
	if !db.Installer {
		return errors.New("grants require the installer database connection")
	}
	if role != "akf_app" {
		return fmt.Errorf("refusing to grant unexpected role %q", role)
	}
	statements := []string{
		"GRANT USAGE ON SCHEMA public TO akf_app",
		`GRANT SELECT, INSERT, UPDATE, DELETE ON
			workspaces, source_connections, source_snapshots, knowledge_items,
			knowledge_search_index, agent_runs, gap_reports, gap_events,
			evidence_links, proposals, validation_runs, evaluation_cases,
			evaluation_runs, audit_events, feedback, webhook_deliveries, query_logs
		TO akf_app`,
		"GRANT SELECT, INSERT, UPDATE ON publication_operations TO akf_app",
		"GRANT SELECT, INSERT, DELETE ON publication_candidates TO akf_app",
		"GRANT SELECT, INSERT ON publication_receipts, knowledge_releases, proposal_merge_links, gap_retrieval_cases TO akf_app",
		"REVOKE ALL ON schema_migrations FROM akf_app",
	}
	for _, statement := range statements {
		if _, err := db.pool.Exec(ctx, statement); err != nil {
			return fmt.Errorf("grant: %w", err)
		}
	}
	return nil
}

// Workspace is a tenant boundary.
type Workspace struct {
	ID            string
	DisplayName   string
	KnowledgeRepo string
	KnowledgeRoot string
	PublishedSHA  string
	RetentionDays int
}

// CreateWorkspace inserts a tenant. Only the installer connection may call it.
func (db *DB) CreateWorkspace(ctx context.Context, ws Workspace) error {
	if !db.Installer {
		return errors.New("create workspace requires the installer database connection")
	}
	if ws.RetentionDays == 0 {
		ws.RetentionDays = 30
	}
	_, err := db.pool.Exec(ctx, `
		INSERT INTO workspaces (
			id, display_name, knowledge_repo, knowledge_root, published_sha, retention_days
		) VALUES ($1, $2, $3, $4, $5, $6)
	`, ws.ID, ws.DisplayName, ws.KnowledgeRepo, ws.KnowledgeRoot, ws.PublishedSHA, ws.RetentionDays)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("workspace %s already exists", ws.ID)
		}
		return fmt.Errorf("insert workspace: %w", err)
	}
	return nil
}

// GetWorkspace reads one workspace under row-level security.
func (db *DB) GetWorkspace(ctx context.Context, id string) (Workspace, error) {
	var ws Workspace
	err := db.WithWorkspace(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT id, display_name, knowledge_repo, knowledge_root, published_sha, retention_days
			FROM workspaces
			WHERE id = $1
		`, id).Scan(&ws.ID, &ws.DisplayName, &ws.KnowledgeRepo, &ws.KnowledgeRoot, &ws.PublishedSHA, &ws.RetentionDays)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Workspace{}, ErrNotFound
	}
	if err != nil {
		return Workspace{}, err
	}
	return ws, nil
}
