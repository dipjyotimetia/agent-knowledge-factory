package okf_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/knowledge/okf"
)

func TestBankingFixture(t *testing.T) {
	bundle, err := okf.Load(filepath.Join(repoRoot(t), "examples/banking/fixture-knowledge"), okf.Options{
		Now: time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bundle.Conformant() {
		t.Fatalf("diagnostics: %+v", bundle.Diagnostics)
	}
	if bundle.Version != "0.3" || bundle.URIPrefix != "kb://banking" {
		t.Fatalf("bundle meta version=%s prefix=%s", bundle.Version, bundle.URIPrefix)
	}
	lifecycle := concept(t, bundle, "events/movement-lifecycle")
	if lifecycle.TrustTier != "human-reviewed" {
		t.Fatalf("trust = %s", lifecycle.TrustTier)
	}
	if !lifecycle.Stale {
		t.Fatal("stale_after in the past was treated as fresh")
	}
	if !strings.Contains(lifecycle.Body, "deduplicate by `movementId`") {
		t.Fatal("fixture lost the intentional stale dedupe rule")
	}
	if lifecycle.Extensions["x-akf-owner"] != "banking-demo" {
		t.Fatalf("extension dropped: %+v", lifecycle.Extensions)
	}
	draft := concept(t, bundle, "events/draft-note")
	if draft.Status != "draft" {
		t.Fatalf("draft status = %s", draft.Status)
	}
	legacy := concept(t, bundle, "events/legacy-json-wire")
	if legacy.Status != "deprecated" || legacy.SupersededBy != "/events/avro-runtime.md" {
		t.Fatalf("legacy = %+v", legacy)
	}
	for _, c := range bundle.Concepts {
		if c.Path == "index.md" || c.Path == "log.md" {
			t.Fatalf("reserved file indexed as concept %s", c.Path)
		}
	}
}

func TestMalformedBundleRejected(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "broken.md"), "---\ntype: [Note\n---\n\nnope\n")
	mustWrite(t, filepath.Join(root, "dup.md"), "---\ntype: Note\ntype: Other\n---\n\n\n")
	mustWrite(t, filepath.Join(root, "open.md"), "---\ntype: Note\n\nno closing fence\n")
	sub := filepath.Join(root, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(sub, "index.md"), "---\ntitle: no\n---\n\n# Nested\n")
	mustWrite(t, filepath.Join(root, "log.md"), "---\ntitle: log\n---\n\n# Log\n")
	bundle, err := okf.Load(root, okf.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Conformant() {
		t.Fatal("malformed bundle was accepted")
	}
}

func TestTrustStaleAndUnknownStatus(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "index.md"), "---\nokf_version: \"0.3\"\n---\n\n# Index\n")
	mustWrite(t, filepath.Join(root, "old-review.md"), `---
type: Note
title: Old review
generated: { by: "agent/rewriter", at: "2026-06-20T00:00:00Z" }
verified: { by: "human:ada", at: "2026-06-01T00:00:00Z" }
status: archived
stale_after: "not-a-date"
---

Body.
`)
	mustWrite(t, filepath.Join(root, "machine.md"), `---
type: Note
generated: { by: "agent/rewriter", at: "2026-06-20T00:00:00Z" }
verified:
  - { by: "process:nightly", at: "2026-06-21T00:00:00Z" }
---

Body.
`)
	mustWrite(t, filepath.Join(root, "compute.md"), `---
type: Attested Computation
title: Missing runtime
---

# Computation

`+"```"+`
select 1
`+"```"+`
`)
	bundle, err := okf.Load(root, okf.Options{Now: time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if !bundle.Conformant() {
		t.Fatalf("unexpected errors: %+v", bundle.Diagnostics)
	}
	old := concept(t, bundle, "old-review")
	if old.TrustTier != "unverified" || !old.StatusUnknown || !old.StaleMalformed {
		t.Fatalf("old-review = trust %s unknown %v staleMalformed %v", old.TrustTier, old.StatusUnknown, old.StaleMalformed)
	}
	if concept(t, bundle, "machine").TrustTier != "machine-confirmed" {
		t.Fatal("process verification was not machine-confirmed")
	}
	compute := concept(t, bundle, "compute")
	if compute.Attestable {
		t.Fatal("computation without runtime was attestable")
	}
}

func TestSymlinkEscapeIsNotRead(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := "SECRET_OUTSIDE_BUNDLE"
	mustWrite(t, filepath.Join(outside, "secret.md"), "---\ntype: Note\n---\n\n"+secret+"\n")
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "leak.md")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "ok.md"), "---\ntype: Note\ntitle: Inside\n---\n\ninside\n")
	bundle, err := okf.Load(root, okf.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Conformant() {
		t.Fatal("escaping symlink was accepted")
	}
	for _, c := range bundle.Concepts {
		if strings.Contains(c.Body, secret) {
			t.Fatal("symlink target was read")
		}
	}
}

func concept(t *testing.T, bundle *okf.Bundle, id string) okf.Concept {
	t.Helper()
	for _, c := range bundle.Concepts {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("missing concept %s", id)
	return okf.Concept{}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
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
