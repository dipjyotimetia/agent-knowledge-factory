package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/testutil"
	"github.com/google/uuid"
)

func Test24TraceImportCommand(t *testing.T) {
	admin, _ := testutil.OpenMigrated(t)
	workspace := "trace-" + uuid.NewString()
	if err := admin.CreateWorkspace(context.Background(), postgres.Workspace{ID: workspace, DisplayName: workspace}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AKF_DATABASE_URL", testutil.AppDSN())
	t.Setenv("AKF_WORKSPACE_ID", "")
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, file := filepath.Join(base, "config.yaml"), filepath.Join(base, "trace.json")
	if err := os.WriteFile(cfg, fmt.Appendf(nil, "workspace:\n  workspace_id: %s\ntraces:\n  operator_actor_id: operator\n  run_owners:\n    run-demo: owner\n", workspace), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`[{"gen_ai.human":"fixture","email":"pii-sentinel@example.test"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(runID, path string) (string, error) {
		cmd := newRoot()
		var out bytes.Buffer
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"traces", "import", "--config", cfg, "--file", path, "--run-id", runID})
		err := cmd.Execute()
		return strings.TrimSuffix(out.String(), "\n"), err
	}
	if got, err := run("unbound", file); err == nil || got != "" {
		t.Fatal("unbound run accepted")
	}
	link := filepath.Join(base, "linked-file")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if got, err := run("run-demo", link); err == nil || got != "" || strings.Contains(err.Error(), base) {
		t.Fatal("symlink was accepted or echoed")
	}
	ref, err := run("run-demo", file)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := domain.TraceID(ref); !ok {
		t.Fatal("import output must contain only the reference")
	}
}
