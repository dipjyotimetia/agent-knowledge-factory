package publish

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// GitHub opens a pull request with a GitHub App or installation token.
// It refuses any URL that would merge.
// Branch is the protected default branch. An empty value means main.
type GitHub struct {
	BaseURL      string
	Owner        string
	Repo         string
	Branch       string
	Installation string
	Token        string
	Tokens       TokenSource
	HTTP         *http.Client
}

// TokenSource supplies a GitHub token. Installation tokens expire, so the
// source may mint a new one.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// CreatePullRequest pushes a branch and opens a pull request. It does not merge.
// A retry for the same branch returns the existing open pull request.
func (g *GitHub) CreatePullRequest(ctx context.Context, req Request) (Result, error) {
	if req.ExpectedBase != "" {
		return g.createPinnedPull(ctx, req)
	}
	if existing, ok, err := g.findOpenPullRequest(ctx, req.Branch); err != nil {
		return Result{}, err
	} else if ok {
		return existing, nil
	}
	branch, err := g.branch()
	if err != nil {
		return Result{}, err
	}
	baseRef, err := g.getJSON(ctx, http.MethodGet, g.repoPath("/git/ref/heads/"+urlQuery(branch)), nil)
	if err != nil {
		return Result{}, err
	}
	baseSHA, _ := nestedString(baseRef, "object", "sha")
	if _, status, err := g.request(ctx, http.MethodPost, g.repoPath("/git/refs"), map[string]any{
		"ref": "refs/heads/" + req.Branch,
		"sha": baseSHA,
	}); err != nil {
		return Result{}, err
	} else if status != http.StatusCreated && status != http.StatusUnprocessableEntity {
		return Result{}, fmt.Errorf("github create ref: %d", status)
	}
	for path, body := range req.Files {
		if err := g.putFile(ctx, path, req.Branch, body); err != nil {
			return Result{}, err
		}
	}
	opened, err := g.getJSON(ctx, http.MethodPost, g.repoPath("/pulls"), map[string]any{
		"title": req.Title,
		"body":  req.Body,
		"head":  req.Branch,
		"base":  branch,
	})
	if err != nil {
		return Result{}, err
	}
	return pullResult(opened, req.Branch), nil
}

// BranchFiles reads the tree at a curator branch. The ref must be a curator branch name.
func (g *GitHub) BranchFiles(ctx context.Context, branch string) (map[string]string, error) {
	if !strings.HasPrefix(branch, "curator/") || strings.Contains(branch, "..") {
		return nil, fmt.Errorf("refusing to read ref %q", branch)
	}
	return g.readTree(ctx, branch)
}

// Tree reads the file contents committed at sha. Callers use it after DefaultHead
// matches that sha, so the indexed bytes are the merged tree.
func (g *GitHub) Tree(ctx context.Context, sha string) (map[string]string, error) {
	if !validSHA(sha) {
		return nil, fmt.Errorf("refusing to read a tree that is not a commit SHA")
	}
	commit, err := g.getJSON(ctx, http.MethodGet, g.repoPath("/git/commits/"+sha), nil)
	if err != nil {
		return nil, err
	}
	tree, _ := nestedString(commit, "tree", "sha")
	if !validSHA(tree) {
		return nil, ErrUnsafeTree
	}
	return g.readTree(ctx, tree)
}

func (g *GitHub) readTree(ctx context.Context, ref string) (map[string]string, error) {
	return g.completeTree(ctx, ref)
}

func (g *GitHub) putFile(ctx context.Context, path, branch, content string) error {
	current, status, err := g.request(ctx, http.MethodGet, g.repoPath("/contents/"+path+"?ref="+urlQuery(branch)), nil)
	if err != nil {
		return err
	}
	body := map[string]any{
		"message": "propose knowledge update",
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"branch":  branch,
	}
	if status == http.StatusOK {
		if sha, _ := current["sha"].(string); sha != "" {
			body["sha"] = sha
		}
	} else if status != http.StatusNotFound {
		return fmt.Errorf("github read %s: %d", path, status)
	}
	_, status, err = g.request(ctx, http.MethodPut, g.repoPath("/contents/"+path), body)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return fmt.Errorf("github write %s: %d", path, status)
	}
	return nil
}

func (g *GitHub) findOpenPullRequest(ctx context.Context, branch string) (Result, bool, error) {
	raw, status, err := g.requestRaw(ctx, http.MethodGet, g.repoPath("/pulls?head="+urlQuery(g.Owner+":"+branch)+"&state=open"), nil)
	if err != nil || status == http.StatusNotFound {
		return Result{}, false, err
	}
	if status != http.StatusOK {
		return Result{}, false, fmt.Errorf("github list pulls: %d", status)
	}
	var pulls []map[string]any
	if err := json.Unmarshal(raw, &pulls); err != nil {
		return Result{}, false, err
	}
	if len(pulls) == 0 {
		return Result{}, false, nil
	}
	return pullResult(pulls[0], branch), true, nil
}

func (g *GitHub) blob(ctx context.Context, sha string) (string, error) {
	payload, err := g.getJSON(ctx, http.MethodGet, g.repoPath("/git/blobs/"+sha), nil)
	if err != nil {
		return "", err
	}
	encoded, ok := payload["content"].(string)
	if !ok || payload["encoding"] != "base64" || payload["sha"] != sha {
		return "", ErrUnsafeTree
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(encoded, "\n", ""))
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func (g *GitHub) repoPath(suffix string) string {
	return fmt.Sprintf("/repos/%s/%s%s", g.Owner, g.Repo, suffix)
}

func pullResult(opened map[string]any, branch string) Result {
	number, _ := opened["number"].(float64)
	url, _ := opened["html_url"].(string)
	head, _ := nestedString(opened, "head", "sha")
	return Result{Number: int(number), URL: url, HeadSHA: head, Branch: branch}
}

func nestedString(object map[string]any, keys ...string) (string, bool) {
	var current any = object
	for _, key := range keys {
		next, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current = next[key]
	}
	value, ok := current.(string)
	return value, ok && value != ""
}

func (g *GitHub) branch() (string, error) {
	branch := g.Branch
	if branch == "" {
		branch = "main"
	}
	if strings.Contains(branch, "..") || strings.ContainsAny(branch, " \t\\") || strings.HasPrefix(branch, "/") {
		return "", fmt.Errorf("invalid default branch %q", branch)
	}
	return branch, nil
}

// DefaultHead reads the configured default branch. It does not change it.
func (g *GitHub) DefaultHead(ctx context.Context) (string, error) {
	branch, err := g.branch()
	if err != nil {
		return "", err
	}
	payload, err := g.getJSON(ctx, "GET", g.repoPath("/git/ref/heads/"+urlQuery(branch)), nil)
	if err != nil {
		return "", err
	}
	sha, _ := nestedString(payload, "object", "sha")
	if sha == "" {
		return "", errors.New("default branch has no sha")
	}
	return sha, nil
}

func (g *GitHub) getJSON(ctx context.Context, method, path string, body any) (map[string]any, error) {
	raw, status, err := g.requestRaw(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, &HTTPError{Method: method, Status: status}
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func (g *GitHub) request(ctx context.Context, method, path string, body any) (map[string]any, int, error) {
	raw, status, err := g.requestRaw(ctx, method, path, body)
	if err != nil || len(raw) == 0 || raw[0] != '{' {
		return map[string]any{}, status, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, status, err
	}
	return decoded, status, nil
}

func (g *GitHub) requestRaw(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	token := g.Token
	if g.Tokens != nil {
		var err error
		token, err = g.Tokens.Token(ctx)
		if err != nil {
			return nil, 0, err
		}
	}
	if strings.HasSuffix(path, "/merge") || strings.Contains(path, "/merges") {
		return nil, 0, errors.New("refusing to merge")
	}
	var buf io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		buf = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(g.BaseURL, "/")+path, buf)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := g.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(raw) > 16<<20 {
		return nil, resp.StatusCode, errors.New("github response exceeds limit")
	}
	return raw, resp.StatusCode, nil
}

func urlQuery(value string) string {
	return url.QueryEscape(value)
}

// HTTPError contains the status and request method without the response body.
type HTTPError struct {
	Method string
	Status int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("github %s: %d", e.Method, e.Status)
}
