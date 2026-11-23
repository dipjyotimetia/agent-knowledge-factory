package skills_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/knowledge/skills"
)

func TestBankingSkillIsValidAndNotExecuted(t *testing.T) {
	marker := "/tmp/akf-banking-skill-executed"
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	root := filepath.Join(repoRoot(t), "examples/banking/fixture-knowledge")
	loaded, err := skills.LoadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("skills = %d", len(loaded))
	}
	skill := loaded[0]
	if !skill.Valid() {
		t.Fatalf("diagnostics: %+v", skill.Diagnostics)
	}
	if skill.Name != "movement-event-dedupe" {
		t.Fatalf("name = %s", skill.Name)
	}
	if skill.Metadata["author"] != "banking-demo" {
		t.Fatalf("metadata = %+v", skill.Metadata)
	}
	if len(skill.Scripts) != 1 || skill.Scripts[0].Path != "scripts/noop.sh" || skill.Scripts[0].Digest == "" {
		t.Fatalf("scripts = %+v", skill.Scripts)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("skill script was executed")
	}
}

func TestRejectsBadNameEscapeAndRunsNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Bad_Name")
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "executed")
	script := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(filepath.Join(dir, "scripts", "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: Bad_Name\ndescription: no\n---\n\nSee [/etc/passwd](/etc/passwd) and [escape](../../outside.md).\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	skill, err := skills.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if skill.Valid() {
		t.Fatal("invalid skill accepted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("script ran during validation")
	}
	joined := diagnostics(skill)
	if !strings.Contains(joined, "name") || !strings.Contains(joined, "/etc/passwd") || !strings.Contains(joined, "escapes") {
		t.Fatalf("diagnostics = %s", joined)
	}
}

func diagnostics(skill *skills.Skill) string {
	out := ""
	for _, d := range skill.Diagnostics {
		out += d.Message + "\n"
	}
	return out
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
