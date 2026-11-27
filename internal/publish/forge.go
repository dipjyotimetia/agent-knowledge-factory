package publish

import (
	"context"
	"crypto/sha1" // #nosec G505 -- GitHub identifies Git objects by SHA-1. Publication receipts use SHA-256.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

var ErrStaleBase = errors.New("forge default head advanced")
var ErrUnsafeTree = errors.New("incomplete or unsafe Git tree")
var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var curatorBranch = regexp.MustCompile(`^curator/[0-9a-f-]{36}$`)

func validSHA(sha string) bool { return shaPattern.MatchString(sha) }

type Identity struct {
	Repository   string
	Installation string
	Branch       string
}

type PullRequest struct {
	Result
	BaseSHA  string
	MergeSHA string
	State    string
}

type Forge interface {
	Publisher
	Resolve(context.Context) (Identity, string, error)
	PullRequest(context.Context, int) (PullRequest, error)
	IsAncestor(context.Context, string, string) (bool, error)
}

func (g *GitHub) Resolve(ctx context.Context) (Identity, string, error) {
	branch, err := g.branch()
	if err != nil {
		return Identity{}, "", err
	}
	repo, err := g.getJSON(ctx, http.MethodGet, g.repoPath(""), nil)
	if err != nil {
		return Identity{}, "", err
	}
	name, _ := repo["full_name"].(string)
	if !strings.EqualFold(name, g.Owner+"/"+g.Repo) || repo["default_branch"] != branch || g.Installation == "" {
		return Identity{}, "", errors.New("forge repository, default branch or installation mismatch")
	}
	// An installation token is scoped to the configured installation. Verify
	// the repository is among the repositories accessible to that token.
	if g.Tokens == nil {
		return Identity{}, "", errors.New("forge identity requires a configured GitHub App")
	}
	if err := g.verifyInstallationRepository(ctx, name, repo["id"]); err != nil {
		return Identity{}, "", err
	}
	head, err := g.DefaultHead(ctx)
	if err != nil {
		return Identity{}, "", err
	}
	if !validSHA(head) {
		return Identity{}, "", errors.New("invalid default head")
	}
	return Identity{strings.ToLower(name), g.Installation, branch}, head, nil
}

func (g *GitHub) verifyInstallationRepository(ctx context.Context, name string, id any) error {
	if _, ok := id.(float64); !ok {
		return errors.New("invalid forge repository ID")
	}
	for page := 1; page <= 20; page++ {
		data, err := g.getJSON(ctx, http.MethodGet, fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page), nil)
		if err != nil {
			return err
		}
		repositories, ok := data["repositories"].([]any)
		if !ok {
			return errors.New("invalid installation repository list")
		}
		for _, value := range repositories {
			repo, ok := value.(map[string]any)
			if ok && repo["id"] == id && repo["full_name"] == name {
				return nil
			}
		}
		if len(repositories) < 100 {
			break
		}
	}
	return errors.New("repository is outside the configured GitHub App installation")
}

func (g *GitHub) PullRequest(ctx context.Context, number int) (PullRequest, error) {
	item, err := g.getJSON(ctx, http.MethodGet, g.repoPath(fmt.Sprintf("/pulls/%d", number)), nil)
	if err != nil {
		return PullRequest{}, err
	}
	pr, err := g.parsePull(item)
	if err != nil {
		return PullRequest{}, err
	}
	if pr.Number != number {
		return PullRequest{}, ErrPRMismatch
	}
	return pr, nil
}

func (g *GitHub) parsePull(item map[string]any) (PullRequest, error) {
	base, _ := nestedString(item, "base", "repo", "full_name")
	head, _ := nestedString(item, "head", "repo", "full_name")
	ref, _ := nestedString(item, "base", "ref")
	branch, err := g.branch()
	if err != nil {
		return PullRequest{}, err
	}
	if !strings.EqualFold(base, g.Owner+"/"+g.Repo) || !strings.EqualFold(head, base) || ref != branch {
		return PullRequest{}, ErrPRMismatch
	}
	headRef, _ := nestedString(item, "head", "ref")
	baseSHA, _ := nestedString(item, "base", "sha")
	pr := PullRequest{Result: pullResult(item, headRef), BaseSHA: baseSHA}
	pr.State, _ = item["state"].(string)
	if merged, _ := item["merged"].(bool); merged {
		pr.MergeSHA, _ = item["merge_commit_sha"].(string)
		if pr.State != "closed" || !validSHA(pr.MergeSHA) {
			return PullRequest{}, errors.New("invalid forge merge identity")
		}
	}
	if pr.Number < 1 || !validSHA(pr.HeadSHA) || !validSHA(baseSHA) || (pr.State != "open" && pr.State != "closed") {
		return PullRequest{}, errors.New("incomplete forge PR")
	}
	return pr, nil
}

func (g *GitHub) IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	if !validSHA(ancestor) || !validSHA(descendant) {
		return false, errors.New("invalid ancestry commits")
	}
	if ancestor == descendant {
		return true, nil
	}
	data, err := g.getJSON(ctx, http.MethodGet, g.repoPath("/compare/"+ancestor+"..."+descendant), nil)
	if err != nil {
		return false, err
	}
	base, _ := nestedString(data, "merge_base_commit", "sha")
	status, _ := data["status"].(string)
	return status == "ahead" && base == ancestor, nil
}

// TreeDigest binds path and bytes independently of map iteration order.
func TreeDigest(files map[string]string) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		_, _ = fmt.Fprintf(hash, "%d:%s%d:%s", len(name), name, len(files[name]), files[name])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func SafeTreePath(name string) bool {
	if name == "" || len(name) > 512 || !utf8.ValidString(name) || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:\x00\r\n") {
		return false
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "." || segment == ".." || strings.EqualFold(segment, ".git") {
			return false
		}
	}
	return true
}

type treeEntry struct {
	Path string
	Mode string
	Type string
	SHA  string
	Size *int
}

func (g *GitHub) treeEntries(ctx context.Context, ref, query string) ([]treeEntry, bool, error) {
	raw, status, err := g.requestRaw(ctx, http.MethodGet, g.repoPath("/git/trees/"+url.PathEscape(ref)+query), nil)
	if err != nil {
		return nil, false, err
	}
	if status != http.StatusOK {
		return nil, false, fmt.Errorf("github tree: %d", status)
	}
	var tree struct {
		Tree      []treeEntry
		Truncated *bool
	}
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, false, err
	}
	if tree.Truncated == nil || tree.Tree == nil {
		return nil, false, ErrUnsafeTree
	}
	return tree.Tree, *tree.Truncated, nil
}

func (g *GitHub) completeTree(ctx context.Context, ref string) (map[string]string, error) {
	entries, truncated, err := g.treeEntries(ctx, ref, "?recursive=1")
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	seen := map[string]bool{}
	total, nodes := 0, 0
	var walk func([]treeEntry, string, int) error
	walk = func(list []treeEntry, prefix string, depth int) error {
		if depth > 20 {
			return ErrUnsafeTree
		}
		for _, entry := range list {
			nodes++
			name := prefix + entry.Path
			key := strings.ToLower(name)
			if nodes > 4000 || strings.Count(name, "/") > 20 || !SafeTreePath(name) || seen[key] || !validSHA(entry.SHA) || (truncated && strings.Contains(entry.Path, "/")) {
				return ErrUnsafeTree
			}
			seen[key] = true
			if entry.Type == "tree" && entry.Mode == "040000" {
				if truncated {
					children, partial, err := g.treeEntries(ctx, entry.SHA, "")
					if err != nil {
						return err
					}
					if partial {
						return ErrUnsafeTree
					}
					if err := walk(children, name+"/", depth+1); err != nil {
						return err
					}
				}
				continue
			}
			if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") || entry.Size == nil || *entry.Size < 0 || *entry.Size > 1<<20 {
				return ErrUnsafeTree
			}
			total += *entry.Size
			if total > 24<<20 {
				return ErrUnsafeTree
			}
			body, err := g.blob(ctx, entry.SHA)
			if err != nil {
				return err
			}
			hash := sha1.New() // #nosec G401 -- Recompute the Git blob ID before comparing with the forge response.
			_, _ = fmt.Fprintf(hash, "blob %d\x00%s", len(body), body)
			if len(body) != *entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.SHA {
				return ErrUnsafeTree
			}
			files[name] = body
		}
		return nil
	}
	if truncated {
		var partial bool
		entries, partial, err = g.treeEntries(ctx, ref, "")
		if err != nil {
			return nil, err
		}
		if partial {
			return nil, ErrUnsafeTree
		}
	}
	if err := walk(entries, "", 0); err != nil {
		return nil, err
	}
	return files, nil
}

func (m *Memory) Resolve(context.Context) (Identity, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Identity{"fixture/knowledge", "fixture", "main"}, m.head, nil
}

func (m *Memory) PullRequest(_ context.Context, number int) (PullRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pr, ok := m.prs[number]
	if !ok {
		return PullRequest{}, errors.New("fixture PR missing")
	}
	result := PullRequest{Result: Result{Number: number, Branch: pr.branch, HeadSHA: pr.head}, BaseSHA: pr.base, MergeSHA: pr.merge, State: "open"}
	if pr.merged {
		result.State = "closed"
	}
	return result, nil
}

func (m *Memory) IsAncestor(_ context.Context, ancestor, descendant string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for sha := descendant; sha != ""; sha = m.parents[sha] {
		if sha == ancestor {
			return true, nil
		}
	}
	return false, nil
}
