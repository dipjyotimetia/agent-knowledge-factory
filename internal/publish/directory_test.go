package publish_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
)

func TestDirectoryWriterStoresASecondFormat(t *testing.T) {
	dir := t.TempDir()
	writer := publish.Directory{Root: dir}
	if err := writer.Write(context.Background(), map[string]string{"events/note.md": "Do not copy rows\n"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "events", "note.md"))
	if err != nil || string(body) != "Do not copy rows\n" {
		t.Fatalf("body %q err %v", body, err)
	}
	if err := writer.Write(context.Background(), map[string]string{"../escape.md": "no"}); err == nil {
		t.Fatal("path escape was accepted")
	}
}
