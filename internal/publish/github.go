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
	"strings"
)

// GitHub opens a pull request with a GitHub App or installation token.
// It refuses any URL that would merge.
type GitHub struct {
	BaseURL string
	Owner   string
	Repo    string
	Token   string
	HTTP    *http.Client
}

// CreatePullRequest pushes a branch and opens a pull request. It does not merge.
func (g *GitHub) CreatePullRequest(ctx context.Context, req Request) (Result, error) {
	baseRef, err := g.getJSON(ctx, "GET", fmt.Sprintf("/repos/%s/%s/git/ref/heads/main", g.Owner, g.Repo), nil)
	if err != nil {
		return Result{}, err
	}
	baseSHA, _ := baseRef["object"].(map[string]any)["sha"].(string)
	if _, err := g.getJSON(ctx, "POST", fmt.Sprintf("/repos/%s/%s/git/refs", g.Owner, g.Repo), map[string]any{
		"ref": "refs/heads/" + req.Branch,
		"sha": baseSHA,
	}); err != nil {
		return Result{}, err
	}
	for path, body := range req.Files {
		if _, err := g.getJSON(ctx, "PUT", fmt.Sprintf("/repos/%s/%s/contents/%s", g.Owner, g.Repo, path), map[string]any{
			"message": "propose knowledge update",
			"content": base64.StdEncoding.EncodeToString([]byte(body)),
			"branch":  req.Branch,
		}); err != nil {
			return Result{}, err
		}
	}
	opened, err := g.getJSON(ctx, "POST", fmt.Sprintf("/repos/%s/%s/pulls", g.Owner, g.Repo), map[string]any{
		"title": req.Title,
		"body":  req.Body,
		"head":  req.Branch,
		"base":  "main",
	})
	if err != nil {
		return Result{}, err
	}
	number, _ := opened["number"].(float64)
	url, _ := opened["html_url"].(string)
	head, _ := opened["head"].(map[string]any)["sha"].(string)
	return Result{Number: int(number), URL: url, HeadSHA: head, Branch: req.Branch}, nil
}

// DefaultHead reads the default branch. It does not change it.
func (g *GitHub) DefaultHead(ctx context.Context) (string, error) {
	payload, err := g.getJSON(ctx, "GET", fmt.Sprintf("/repos/%s/%s/git/ref/heads/main", g.Owner, g.Repo), nil)
	if err != nil {
		return "", err
	}
	sha, _ := payload["object"].(map[string]any)["sha"].(string)
	if sha == "" {
		return "", errors.New("default branch has no sha")
	}
	return sha, nil
}

func (g *GitHub) getJSON(ctx context.Context, method, path string, body any) (map[string]any, error) {
	if strings.HasSuffix(path, "/merge") || strings.Contains(path, "/merges") {
		return nil, errors.New("refusing to merge")
	}
	var buf io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		buf = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(g.BaseURL, "/")+path, buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
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
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github %s %s: %d", method, path, resp.StatusCode)
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
