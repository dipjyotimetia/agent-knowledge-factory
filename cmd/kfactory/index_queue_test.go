package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/testutil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func Test22CLIIndexReturnsDurableOperation(t *testing.T) {
	admin, app := testutil.OpenMigrated(t)
	ctx := context.Background()
	ws := "index-cli-" + uuid.NewString()
	if err := admin.CreateWorkspace(ctx, postgres.Workspace{ID: ws, DisplayName: ws, KnowledgeRepo: "fixture/knowledge"}); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.yaml")
	data := fmt.Appendf(nil, "workspace:\n  workspace_id: %s\n  knowledge_owner: fixture\n  knowledge_repo: knowledge\n", ws)
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AKF_DATABASE_URL", testutil.AppDSN())
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "42")
	t.Setenv("GITHUB_KNOWLEDGE_OWNER", "")
	t.Setenv("GITHUB_KNOWLEDGE_REPO", "")
	cmd := newIndexCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--config", config, "--bootstrap"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	id, err := uuid.Parse(strings.TrimSpace(strings.TrimPrefix(out.String(), "pending ")))
	if err != nil {
		t.Fatalf("pending operation %q", out.String())
	}
	if err := app.WithWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		var op, state, published string
		err := tx.QueryRow(ctx, `SELECT o.trigger,o.state,w.published_sha FROM publication_operations o
		  JOIN workspaces w ON w.id=o.workspace_id WHERE o.id=$1`, id).Scan(&op, &state, &published)
		if err == nil && (op != "bootstrap" || state != "queued" || published != "") {
			t.Errorf("unexpected state %q %q %q", op, state, published)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
