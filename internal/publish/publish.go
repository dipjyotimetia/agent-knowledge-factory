// Package publish opens a pull request. It has no merge operation.
// A human merges in the forge. The factory only observes the resulting head.
package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Request is a branch and a set of full file contents.
type Request struct {
	Title  string
	Body   string
	Branch string
	Files  map[string]string
}

// Result identifies the opened pull request. It is not a merge.
type Result struct {
	Number  int
	URL     string
	HeadSHA string
	Branch  string
}

// Publisher creates a pull request and reads heads. Merge is intentionally absent.
// Tree returns the file overlay at a commit on the default branch.
type Publisher interface {
	CreatePullRequest(ctx context.Context, req Request) (Result, error)
	DefaultHead(ctx context.Context) (string, error)
	Tree(ctx context.Context, sha string) (map[string]string, error)
	BranchFiles(ctx context.Context, branch string) (map[string]string, error)
}

// ErrNotMerged means the default branch is not the claimed merge commit.
var ErrNotMerged = errors.New("merge commit is not the default branch head")

// Memory is an in-process forge. HumanMerge is not part of Publisher.
type Memory struct {
	mu        sync.Mutex
	head      string
	files     map[string]string
	prs       map[int]*pull
	next      int
	snapshots map[string]map[string]string
}

type pull struct {
	number int
	branch string
	files  map[string]string
	head   string
	merged bool
}

// NewMemory returns a forge whose default branch is sha and files.
func NewMemory(sha string, files map[string]string) *Memory {
	copied := map[string]string{}
	for k, v := range files {
		copied[k] = v
	}
	if sha == "" {
		sha = digest(copied)
	}
	return &Memory{head: sha, files: copied, prs: map[int]*pull{}, snapshots: map[string]map[string]string{}}
}

// CreatePullRequest opens a pull request without changing the default branch.
func (m *Memory) CreatePullRequest(_ context.Context, req Request) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if req.Branch == "" || len(req.Files) == 0 {
		return Result{}, errors.New("branch and files are required")
	}
	for _, existing := range m.prs {
		if existing.branch == req.Branch {
			return Result{
				Number:  existing.number,
				URL:     fmt.Sprintf("https://example.invalid/pull/%d", existing.number),
				HeadSHA: existing.head,
				Branch:  existing.branch,
			}, nil
		}
	}
	m.next++
	head := digest(req.Files)
	m.prs[m.next] = &pull{number: m.next, branch: req.Branch, files: clone(req.Files), head: head}
	return Result{
		Number:  m.next,
		URL:     fmt.Sprintf("https://example.invalid/pull/%d", m.next),
		HeadSHA: head,
		Branch:  req.Branch,
	}, nil
}

// DefaultHead is the merge commit on the default branch, not a pull-request head.
func (m *Memory) DefaultHead(context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.head, nil
}

// BranchFiles returns the open pull request contents for a branch.
func (m *Memory) BranchFiles(_ context.Context, branch string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, pr := range m.prs {
		if pr.branch == branch && !pr.merged {
			return clone(pr.files), nil
		}
	}
	return nil, fmt.Errorf("no open pull request for %s", branch)
}

// Tree returns the files merged at sha. The result is the pull-request overlay,
// not the proposal retained by the caller.
func (m *Memory) Tree(_ context.Context, sha string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	files, ok := m.snapshots[sha]
	if !ok {
		return nil, fmt.Errorf("%w: no tree for %s", ErrNotMerged, sha)
	}
	return clone(files), nil
}

// Revise changes a file on an open pull request before a human merges it.
func (m *Memory) Revise(number int, path, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pr, ok := m.prs[number]
	if !ok || pr.merged {
		return fmt.Errorf("pull request %d is not open", number)
	}
	pr.files[path] = body
	pr.head = digest(pr.files)
	return nil
}

// Files returns the default-branch contents.
func (m *Memory) Files() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return clone(m.files)
}

// HumanMerge simulates a person merging the pull request. Curator code cannot call it
// through the Publisher interface.
func (m *Memory) HumanMerge(number int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pr, ok := m.prs[number]
	if !ok {
		return "", fmt.Errorf("pull request %d not found", number)
	}
	overlay := clone(pr.files)
	for path, body := range overlay {
		m.files[path] = body
	}
	pr.merged = true
	m.head = digest(m.files)
	m.snapshots[m.head] = overlay
	return m.head, nil
}

func clone(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func digest(files map[string]string) string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(files[k]))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:40]
}
