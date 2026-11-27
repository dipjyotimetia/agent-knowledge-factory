//go:build linux || darwin

package traces_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/traces"
	"golang.org/x/sys/unix"
)

func Test24LocalFileTraversal(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw := `[{"gen_ai.human":"fixture"}]`
	path := filepath.Join(base, "trace.json")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(base, "file-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(base, filepath.Join(base, "dir-link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(base, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "large"), []byte(strings.Repeat(" ", traces.MaxBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := traces.ReadLocal(path); err != nil || string(got) != raw {
		t.Fatalf("regular file: %v", err)
	}
	for _, unsafe := range []string{"file-link", "dir-link/trace.json", "fifo", ".", "large", "absent", "dir-link/../trace.json"} {
		if _, err := traces.ReadLocal(base + "/" + unsafe); err == nil || strings.Contains(err.Error(), base) {
			t.Errorf("unsafe path %q returned %v", unsafe, err)
		}
	}
}
