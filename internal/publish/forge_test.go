package publish

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type fixtureInstallation string

func (f fixtureInstallation) Token(context.Context) (string, error) {
	return string(f), nil
}

func Test22CompleteImmutableTreeAndUnsafeSubtree(t *testing.T) {
	ctx := context.Background()
	sha := strings.Repeat("a", 40)
	root := strings.Repeat("b", 40)
	subtree := strings.Repeat("c", 40)
	body := "subtree contents"
	blob := sha1.Sum(fmt.Appendf(nil, "blob %d\x00%s", len(body), body))
	digest := hex.EncodeToString(blob[:])
	var unsafe atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		switch r.URL.Path {
		case "/repos/fixture/knowledge/git/commits/" + sha:
			write(map[string]any{"tree": map[string]string{"sha": root}})
		case "/repos/fixture/knowledge/git/trees/" + root:
			if r.URL.Query().Has("recursive") {
				write(map[string]any{"truncated": true, "tree": []any{}})
			} else {
				write(map[string]any{"truncated": false, "tree": []map[string]string{{"path": "notes", "type": "tree", "mode": "040000", "sha": subtree}}})
			}
		case "/repos/fixture/knowledge/git/trees/" + subtree:
			mode, name := "100644", "note.md"
			if unsafe.Load() {
				mode, name = "120000", "../escape"
			}
			write(map[string]any{"truncated": false, "tree": []map[string]any{{"path": name, "type": "blob", "mode": mode, "sha": digest, "size": len(body)}}})
		case "/repos/fixture/knowledge/git/blobs/" + digest:
			write(map[string]string{"sha": digest, "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(body))})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	forge := &GitHub{BaseURL: server.URL, Owner: "fixture", Repo: "knowledge"}
	files, err := forge.Tree(ctx, sha)
	if err != nil || len(files) != 1 || files["notes/note.md"] != body {
		t.Fatalf("full tree %v %v", files, err)
	}
	unsafe.Store(true)
	if _, err := forge.Tree(ctx, sha); !errors.Is(err, ErrUnsafeTree) {
		t.Fatalf("unsafe subtree accepted: %v", err)
	}
}

func Test22ForgeIdentityExactPRAndAncestry(t *testing.T) {
	ctx := context.Background()
	base := strings.Repeat("a", 40)
	head := strings.Repeat("b", 40)
	var installed atomic.Bool
	installed.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		repo := map[string]any{"id": 42, "full_name": "fixture/knowledge", "default_branch": "main"}
		switch r.URL.Path {
		case "/repos/fixture/knowledge":
			write(repo)
		case "/installation/repositories":
			repos := []any{}
			if installed.Load() {
				repos = append(repos, repo)
			}
			write(map[string]any{"repositories": repos})
		case "/repos/fixture/knowledge/git/ref/heads/main":
			write(map[string]any{"object": map[string]string{"sha": head}})
		case "/repos/fixture/knowledge/pulls/7":
			write(map[string]any{"number": 7, "state": "closed", "merged": true, "merge_commit_sha": head,
				"base": map[string]any{"repo": repo, "ref": "main", "sha": base},
				"head": map[string]any{"repo": repo, "ref": "curator/example", "sha": head},
			})
		case "/repos/fixture/knowledge/compare/" + base + "..." + head:
			write(map[string]any{"status": "ahead", "merge_base_commit": map[string]string{"sha": base}})
		case "/repos/fixture/knowledge/pulls":
			write([]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	forge := &GitHub{BaseURL: server.URL, Owner: "fixture", Repo: "knowledge", Installation: "4", Tokens: fixtureInstallation("fixture")}
	identity, sha, err := forge.Resolve(ctx)
	if err != nil || identity != (Identity{"fixture/knowledge", "4", "main"}) || sha != head {
		t.Fatalf("resolve %+v %s %v", identity, sha, err)
	}
	pr, err := forge.PullRequest(ctx, 7)
	if err != nil || pr.MergeSHA != head || pr.BaseSHA != base || pr.Branch != "curator/example" {
		t.Fatalf("pull %+v %v", pr, err)
	}
	if yes, err := forge.IsAncestor(ctx, base, head); err != nil || !yes {
		t.Fatalf("ancestor %t %v", yes, err)
	}
	request := Request{ExpectedBase: base, TreeDigest: "digest", Branch: "curator/aabbccdd-aabb-aabb-aabb-aabbccddeeff",
		Files: map[string]string{"note.md": "change"}}
	if _, err := forge.CreatePullRequest(ctx, request); !errors.Is(err, ErrStaleBase) {
		t.Fatalf("stale base: %v", err)
	}
	installed.Store(false)
	if _, _, err := forge.Resolve(ctx); err == nil {
		t.Fatal("repository outside installation accepted")
	}
}

func TestPinnedPullRecoversOnlyAnIdenticalRequest(t *testing.T) {
	ctx := context.Background()
	prHead, base, tree := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	current := strings.Repeat("d", 40)
	content := "pinned content"
	branch := "curator/aabbccdd-aabb-aabb-aabb-aabbccddeeff"
	sum := sha1.Sum(fmt.Appendf(nil, "blob %d\x00%s", len(content), content))
	blob := hex.EncodeToString(sum[:])
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
			http.NotFound(w, r)
			return
		}
		write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		repo := map[string]any{"id": 42, "full_name": "fixture/knowledge", "default_branch": "main"}
		switch r.URL.Path {
		case "/repos/fixture/knowledge":
			write(repo)
		case "/installation/repositories":
			write(map[string]any{"repositories": []any{repo}})
		case "/repos/fixture/knowledge/git/ref/heads/main":
			write(map[string]any{"object": map[string]string{"sha": current}})
		case "/repos/fixture/knowledge/pulls":
			write([]any{map[string]any{"number": 7, "state": "open",
				"base": map[string]any{"repo": repo, "ref": "main", "sha": current},
				"head": map[string]any{"repo": repo, "ref": branch, "sha": prHead},
			}})
		case "/repos/fixture/knowledge/git/commits/" + prHead:
			write(map[string]any{"tree": map[string]string{"sha": tree}, "parents": []map[string]string{{"sha": base}}})
		case "/repos/fixture/knowledge/git/trees/" + tree:
			write(map[string]any{"truncated": false, "tree": []map[string]any{
				{"path": "note.md", "type": "blob", "mode": "100644", "sha": blob, "size": len(content)},
			}})
		case "/repos/fixture/knowledge/git/blobs/" + blob:
			write(map[string]any{"sha": blob, "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content))})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	forge := &GitHub{BaseURL: server.URL, Owner: "fixture", Repo: "knowledge", Installation: "4", Tokens: fixtureInstallation("fixture")}
	files := map[string]string{"note.md": content}
	req := Request{ExpectedBase: base, TreeDigest: TreeDigest(files), Branch: branch, Files: files}
	if pr, err := forge.CreatePullRequest(ctx, req); err != nil || pr.Number != 7 {
		t.Fatalf("recover exact PR: %+v %v", pr, err)
	}
	req.TreeDigest = TreeDigest(map[string]string{"note.md": "mismatched contents"})
	if _, err := forge.CreatePullRequest(ctx, req); !errors.Is(err, ErrPRMismatch) {
		t.Fatalf("foreign contents accepted: %v", err)
	}
	if writes.Load() != 0 {
		t.Fatal("recovery made a forge write")
	}
}
