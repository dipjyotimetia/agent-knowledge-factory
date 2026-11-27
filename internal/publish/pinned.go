package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/policy"
	"net/http"
	"net/url"
)

// ErrPRMismatch means the existing branch or PR cannot be associated with the request.
var ErrPRMismatch = errors.New("forge PR does not match the pinned request")

// ErrPRBaseAdvanced confirms that the base moved before a pull request was posted.
var ErrPRBaseAdvanced = fmt.Errorf("PR preflight: %w", ErrStaleBase)

func (g *GitHub) createPinnedPull(ctx context.Context, req Request) (Result, error) {
	if !curatorBranch.MatchString(req.Branch) || !validSHA(req.ExpectedBase) || req.TreeDigest == "" || len(req.Files) == 0 ||
		len(req.Title) > 256 || len(req.Body) > 65536 || policy.Redact(req.Title) != req.Title || policy.Redact(req.Body) != req.Body {
		return Result{}, ErrPRMismatch
	}
	identity, base, err := g.Resolve(ctx)
	if err != nil {
		return Result{}, err
	}
	// A lost response can be recovered even after the base advanced.
	raw, status, err := g.requestRaw(ctx, http.MethodGet, g.repoPath("/pulls?head="+urlQuery(g.Owner+":"+req.Branch)+"&state=all"), nil)
	if err != nil {
		return Result{}, err
	}
	if status != http.StatusOK {
		return Result{}, fmt.Errorf("github list pulls: %d", status)
	}
	var pulls []map[string]any
	if err := json.Unmarshal(raw, &pulls); err != nil {
		return Result{}, err
	}
	if len(pulls) > 0 {
		if len(pulls) != 1 {
			return Result{}, ErrPRMismatch
		}
		pr, err := g.parsePull(pulls[0])
		if err != nil {
			return Result{}, err
		}
		if pr.Branch != req.Branch {
			return Result{}, ErrPRMismatch
		}
		return pr.Result, g.checkProposalCommit(ctx, pr.HeadSHA, req)
	}
	if base != req.ExpectedBase {
		return Result{}, ErrPRBaseAdvanced
	}
	refPath := g.repoPath("/git/ref/heads/" + url.PathEscape(req.Branch))
	existing, status, err := g.request(ctx, http.MethodGet, refPath, nil)
	if err != nil {
		return Result{}, err
	}
	switch status {
	case http.StatusOK:
		head, _ := nestedString(existing, "object", "sha")
		if err := g.checkProposalCommit(ctx, head, req); err != nil {
			return Result{}, err
		}
	case http.StatusNotFound:
		if err := g.createProposalRef(ctx, req); err != nil {
			return Result{}, err
		}
	default:
		return Result{}, fmt.Errorf("github read proposal ref: %d", status)
	}
	newest, err := g.DefaultHead(ctx)
	if err != nil {
		return Result{}, err
	}
	if newest != base {
		return Result{}, ErrPRBaseAdvanced
	}
	opened, err := g.getJSON(ctx, http.MethodPost, g.repoPath("/pulls"), map[string]string{
		"title": req.Title, "body": req.Body, "head": req.Branch, "base": identity.Branch,
	})
	if err != nil {
		return Result{}, err
	}
	pr, err := g.parsePull(opened)
	if err != nil {
		return Result{}, err
	}
	if pr.BaseSHA != base || pr.Branch != req.Branch {
		return Result{}, ErrPRMismatch
	}
	return pr.Result, g.checkProposalCommit(ctx, pr.HeadSHA, req)
}

// HasPullRequest checks all PR states for the exact same-repository branch.
func (g *GitHub) HasPullRequest(ctx context.Context, branch string) (bool, error) {
	if !curatorBranch.MatchString(branch) {
		return false, ErrPRMismatch
	}
	raw, status, err := g.requestRaw(ctx, http.MethodGet, g.repoPath("/pulls?head="+urlQuery(g.Owner+":"+branch)+"&state=all"), nil)
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, &HTTPError{Method: http.MethodGet, Status: status}
	}
	var pulls []json.RawMessage
	if err := json.Unmarshal(raw, &pulls); err != nil {
		return false, err
	}
	if pulls == nil {
		return false, ErrPRMismatch
	}
	return len(pulls) > 0, nil
}

func (g *GitHub) checkProposalCommit(ctx context.Context, sha string, req Request) error {
	if !validSHA(sha) {
		return ErrPRMismatch
	}
	commit, err := g.getJSON(ctx, http.MethodGet, g.repoPath("/git/commits/"+sha), nil)
	if err != nil {
		return err
	}
	parents, _ := commit["parents"].([]any)
	if len(parents) != 1 {
		return ErrPRMismatch
	}
	parent, _ := parents[0].(map[string]any)
	if parent["sha"] != req.ExpectedBase {
		return ErrPRMismatch
	}
	files, err := g.Tree(ctx, sha)
	if err != nil {
		return err
	}
	if TreeDigest(files) != req.TreeDigest {
		return ErrPRMismatch
	}
	return nil
}

func (g *GitHub) createProposalRef(ctx context.Context, req Request) error {
	base, err := g.getJSON(ctx, http.MethodGet, g.repoPath("/git/commits/"+req.ExpectedBase), nil)
	if err != nil {
		return err
	}
	baseTree, _ := nestedString(base, "tree", "sha")
	if !validSHA(baseTree) {
		return ErrUnsafeTree
	}
	entries := make([]map[string]string, 0, len(req.Files))
	for name, content := range req.Files {
		if !SafeTreePath(name) {
			return ErrUnsafeTree
		}
		entries = append(entries, map[string]string{"path": name, "type": "blob", "mode": "100644", "content": content})
	}
	tree, err := g.getJSON(ctx, http.MethodPost, g.repoPath("/git/trees"), map[string]any{"base_tree": baseTree, "tree": entries})
	if err != nil {
		return err
	}
	treeSHA, _ := tree["sha"].(string)
	if !validSHA(treeSHA) {
		return ErrUnsafeTree
	}
	commit, err := g.getJSON(ctx, http.MethodPost, g.repoPath("/git/commits"), map[string]any{
		"message": "Propose knowledge update", "tree": treeSHA, "parents": []string{req.ExpectedBase},
	})
	if err != nil {
		return err
	}
	sha, _ := commit["sha"].(string)
	if err := g.checkProposalCommit(ctx, sha, req); err != nil {
		return err
	}
	_, err = g.getJSON(ctx, http.MethodPost, g.repoPath("/git/refs"), map[string]string{"ref": "refs/heads/" + req.Branch, "sha": sha})
	return err
}
