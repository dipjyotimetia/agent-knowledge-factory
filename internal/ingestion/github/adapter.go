// Package github fetches pinned files from GitHub. It is read-only: there is
// no method that creates, updates, or merges anything.
package github

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL = "https://api.github.com"
	maxBytes       = 2 << 20
	maxRedirects   = 2
)

// ErrNotAllowlisted means the host, path, or ref is outside the configured scope.
var ErrNotAllowlisted = errors.New("source is not allowlisted")

// ErrRateLimited means GitHub asked the client to stop or the recorded budget is spent.
var ErrRateLimited = errors.New("github rate limit")

// ErrNotFound means the pinned path does not exist.
var ErrNotFound = errors.New("source not found")

// Allowlist is the only set of hosts, paths, and commit SHAs the adapter will fetch.
type Allowlist struct {
	Hosts         []string
	Owner         string
	Repo          string
	PathPrefixes  []string
	Refs          []string
	ClassByPrefix map[string]string
}

// ACL is the repository visibility observed with the snapshot.
type ACL struct {
	Visibility string
	Private    bool
}

// Snapshot is one pinned file. Digest is sha256 of the raw bytes, not the Git blob id.
type Snapshot struct {
	Location       string
	Revision       string
	Digest         string
	Content        []byte
	ETag           string
	NotModified    bool
	ACL            ACL
	Classification string
}

// Adapter reads the GitHub contents API.
type Adapter struct {
	BaseURL    string
	HTTP       *http.Client
	Token      string
	Allow      Allowlist
	PinSHA     string
	MaxRetries int

	mu        sync.Mutex
	exhausted bool
	repoACL   *ACL
}

// New returns an adapter that talks only to allowlisted https hosts
// (http is accepted for 127.0.0.1 test servers).
func New(allow Allowlist, pinSHA string) *Adapter {
	a := &Adapter{
		BaseURL:    defaultBaseURL,
		Allow:      allow,
		PinSHA:     pinSHA,
		MaxRetries: 2,
	}
	a.HTTP = &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: a.checkRedirect,
	}
	return a
}

// Fetch loads path at a commit SHA. etag may be empty. A 304 sets NotModified
// and leaves Content empty so the caller keeps the copy it already verified.
func (a *Adapter) Fetch(ctx context.Context, path, rev, etag string) (Snapshot, error) {
	if a.exhaustedNow() {
		return Snapshot{}, ErrRateLimited
	}
	if err := a.authorize(path, rev); err != nil {
		return Snapshot{}, err
	}
	acl, err := a.visibility(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	endpoint, err := a.contentsURL(path, rev)
	if err != nil {
		return Snapshot{}, err
	}
	body, header, status, err := a.get(ctx, endpoint, etag)
	if err != nil {
		return Snapshot{}, err
	}
	if status == http.StatusNotModified {
		return Snapshot{
			Location:       a.location(path),
			Revision:       rev,
			ETag:           header.Get("ETag"),
			NotModified:    true,
			ACL:            acl,
			Classification: a.classify(path),
		}, nil
	}
	if status == http.StatusNotFound {
		return Snapshot{}, ErrNotFound
	}
	if status != http.StatusOK {
		return Snapshot{}, fmt.Errorf("github contents: status %d", status)
	}
	content, err := decodeContents(body)
	if err != nil {
		return Snapshot{}, err
	}
	sum := sha256.Sum256(content)
	return Snapshot{
		Location:       a.location(path),
		Revision:       rev,
		Digest:         "sha256:" + hex.EncodeToString(sum[:]),
		Content:        content,
		ETag:           header.Get("ETag"),
		ACL:            acl,
		Classification: a.classify(path),
	}, nil
}

func (a *Adapter) authorize(path, rev string) error {
	if !isHexSHA(rev) {
		return fmt.Errorf("%w: ref must be a 40-character commit SHA", ErrNotAllowlisted)
	}
	if rev != a.PinSHA && !contains(a.Allow.Refs, rev) {
		return fmt.Errorf("%w: ref %s", ErrNotAllowlisted, rev)
	}
	cleaned := path
	if strings.Contains(cleaned, "\\") || strings.Contains(cleaned, "..") || strings.HasPrefix(cleaned, "/") {
		return fmt.Errorf("%w: path %s", ErrNotAllowlisted, path)
	}
	if !hasPrefix(a.Allow.PathPrefixes, cleaned) {
		return fmt.Errorf("%w: path %s", ErrNotAllowlisted, path)
	}
	if a.Allow.Owner == "" || a.Allow.Repo == "" {
		return fmt.Errorf("%w: owner and repo are required", ErrNotAllowlisted)
	}
	return nil
}

func (a *Adapter) contentsURL(path, rev string) (*url.URL, error) {
	base, err := url.Parse(a.base())
	if err != nil {
		return nil, err
	}
	if !schemeAllowed(base) || !a.hostAllowed(base) {
		return nil, fmt.Errorf("%w: base host %s", ErrNotAllowlisted, base.Host)
	}
	ref := &url.URL{
		Scheme: base.Scheme,
		Host:   base.Host,
		Path:   fmt.Sprintf("/repos/%s/%s/contents/%s", url.PathEscape(a.Allow.Owner), url.PathEscape(a.Allow.Repo), escapePath(path)),
	}
	q := ref.Query()
	q.Set("ref", rev)
	ref.RawQuery = q.Encode()
	return ref, nil
}

func (a *Adapter) visibility(ctx context.Context) (ACL, error) {
	a.mu.Lock()
	cached := a.repoACL
	a.mu.Unlock()
	if cached != nil {
		return *cached, nil
	}
	base, err := url.Parse(a.base())
	if err != nil {
		return ACL{}, err
	}
	if !schemeAllowed(base) || !a.hostAllowed(base) {
		return ACL{}, fmt.Errorf("%w: base host %s", ErrNotAllowlisted, base.Host)
	}
	endpoint := &url.URL{
		Scheme: base.Scheme,
		Host:   base.Host,
		Path:   fmt.Sprintf("/repos/%s/%s", url.PathEscape(a.Allow.Owner), url.PathEscape(a.Allow.Repo)),
	}
	body, _, status, err := a.get(ctx, endpoint, "")
	if err != nil {
		return ACL{}, err
	}
	if status != http.StatusOK {
		return ACL{}, fmt.Errorf("github repo: status %d", status)
	}
	var payload struct {
		Private    bool   `json:"private"`
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ACL{}, fmt.Errorf("github repo: %w", err)
	}
	if payload.Visibility == "" {
		if payload.Private {
			payload.Visibility = "private"
		} else {
			payload.Visibility = "public"
		}
	}
	acl := ACL{Visibility: payload.Visibility, Private: payload.Private}
	a.mu.Lock()
	a.repoACL = &acl
	a.mu.Unlock()
	return acl, nil
}

func (a *Adapter) get(ctx context.Context, endpoint *url.URL, etag string) ([]byte, http.Header, int, error) {
	var lastErr error
	attempts := a.MaxRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	for try := 0; try < attempts; try++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, nil, 0, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		if a.Token != "" {
			req.Header.Set("Authorization", "Bearer "+a.Token)
		}
		resp, err := a.client().Do(req)
		if err != nil {
			lastErr = err
			if !retryable(err) || try == attempts-1 {
				return nil, nil, 0, sanitize(err, a.Token)
			}
			continue
		}
		body, readErr := readLimited(resp)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, nil, resp.StatusCode, readErr
		}
		a.noteRate(resp.Header)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden && remainingZero(resp.Header) {
			if try == attempts-1 {
				return nil, nil, resp.StatusCode, ErrRateLimited
			}
			wait(ctx, resp.Header)
			continue
		}
		return body, resp.Header.Clone(), resp.StatusCode, nil
	}
	if lastErr == nil {
		lastErr = ErrRateLimited
	}
	return nil, nil, 0, sanitize(lastErr, a.Token)
}

func (a *Adapter) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return errors.New("too many redirects")
	}
	if !schemeAllowed(req.URL) || !a.hostAllowed(req.URL) {
		return fmt.Errorf("%w: redirect host %s", ErrNotAllowlisted, req.URL.Host)
	}
	return nil
}

func (a *Adapter) hostAllowed(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	for _, allowed := range a.Allow.Hosts {
		if strings.EqualFold(allowed, host) || strings.EqualFold(allowed, u.Host) {
			return true
		}
	}
	return false
}

func (a *Adapter) classify(path string) string {
	best := "secondary"
	bestLen := -1
	for prefix, class := range a.Allow.ClassByPrefix {
		if strings.HasPrefix(path, prefix) && len(prefix) > bestLen {
			best = class
			bestLen = len(prefix)
		}
	}
	return best
}

func (a *Adapter) location(path string) string {
	return "github://" + a.Allow.Owner + "/" + a.Allow.Repo + "/" + path
}

func (a *Adapter) base() string {
	if a.BaseURL == "" {
		return defaultBaseURL
	}
	return a.BaseURL
}

func (a *Adapter) client() *http.Client {
	if a.HTTP == nil {
		return http.DefaultClient
	}
	return a.HTTP
}

func (a *Adapter) exhaustedNow() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.exhausted
}

func (a *Adapter) noteRate(h http.Header) {
	if remainingZero(h) {
		a.mu.Lock()
		a.exhausted = true
		a.mu.Unlock()
	}
}

func remainingZero(h http.Header) bool {
	v := h.Get("X-RateLimit-Remaining")
	return v == "0"
}

func wait(ctx context.Context, h http.Header) {
	seconds, _ := strconv.Atoi(h.Get("Retry-After"))
	if seconds < 0 {
		seconds = 0
	}
	if seconds > 2 {
		seconds = 2
	}
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func retryable(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "connection reset")
}

func sanitize(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	if strings.Contains(err.Error(), token) {
		return errors.New("github request failed")
	}
	return err
}

func readLimited(resp *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBytes {
		return nil, fmt.Errorf("github response exceeds %d bytes", maxBytes)
	}
	return body, nil
}

func decodeContents(body []byte) ([]byte, error) {
	var payload struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		Size     int    `json:"size"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("github contents: %w", err)
	}
	if payload.Type != "" && payload.Type != "file" {
		return nil, fmt.Errorf("github contents: type %s", payload.Type)
	}
	if payload.Size > maxBytes {
		return nil, fmt.Errorf("github contents: size %d", payload.Size)
	}
	if payload.Encoding != "" && payload.Encoding != "base64" {
		return nil, fmt.Errorf("github contents: encoding %s", payload.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(payload.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("github contents: %w", err)
	}
	if len(decoded) > maxBytes {
		return nil, fmt.Errorf("github contents: decoded size %d", len(decoded))
	}
	return decoded, nil
}

func schemeAllowed(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	host := u.Hostname()
	return u.Scheme == "http" && (host == "127.0.0.1" || host == "localhost")
}

func isHexSHA(rev string) bool {
	if len(rev) != 40 {
		return false
	}
	for _, r := range rev {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

func hasPrefix(prefixes []string, path string) bool {
	for _, prefix := range prefixes {
		if prefix != "" && strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func escapePath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
