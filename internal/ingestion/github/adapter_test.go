package github_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/ingestion/github"
)

const pin = "8e6680d683ac728f0f089bd60e4a96576ab4ed45"

func TestFetchPinnedContract(t *testing.T) {
	readme := mustRead(t, "readme.md")
	avsc := mustRead(t, "movement-lifecycle-event.avsc")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.URL.Path == "/repos/dipjyotimetia/distributed-systems":
			_ = json.NewEncoder(w).Encode(map[string]any{"private": false, "visibility": "public"})
		case strings.Contains(r.URL.Path, "README.md"):
			if r.URL.Query().Get("ref") != pin {
				t.Errorf("ref = %s", r.URL.Query().Get("ref"))
			}
			writeContents(w, readme)
		case strings.Contains(r.URL.Path, "movement-lifecycle-event.avsc"):
			writeContents(w, avsc)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	adapter := newAdapter(t, srv.URL)
	snap, err := adapter.Fetch(context.Background(), "contracts/avro/README.md", pin, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(snap.Content) != string(readme) {
		t.Fatal("recorded README was not returned byte for byte")
	}
	if !strings.HasPrefix(snap.Digest, "sha256:") || snap.ACL.Visibility != "public" || snap.ACL.Private {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Classification != "contract" {
		t.Fatalf("classification = %s", snap.Classification)
	}
	if snap.Location != "github://dipjyotimetia/distributed-systems/contracts/avro/README.md" {
		t.Fatalf("location = %s", snap.Location)
	}
	if !strings.Contains(string(snap.Content), "deduplicate by `eventId`") {
		t.Fatal("recorded contract lost the eventId dedupe rule")
	}
	schema, err := adapter.Fetch(context.Background(), "contracts/avro/movement-lifecycle-event.avsc", pin, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schema.Content), `"name": "eventId"`) {
		t.Fatal("recorded schema lost eventId")
	}
}

func TestRefusesPathsOutsideTheAllowlistWithoutARequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(srv.Close)
	adapter := newAdapter(t, srv.URL)
	cases := []struct {
		path string
		rev  string
	}{
		{"backend/payments-service/src/Main.java", pin},
		{"contracts/avro/../../secrets.env", pin},
		{"/etc/passwd", pin},
		{"contracts/avro/README.md", "main"},
		{"contracts/avro/README.md", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	for _, tc := range cases {
		_, err := adapter.Fetch(context.Background(), tc.path, tc.rev, "")
		if !errorsIsAllow(err) {
			t.Fatalf("Fetch(%s, %s) err = %v", tc.path, tc.rev, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("disallowed fetches hit the network %d times", hits.Load())
	}
}

func TestRedirectToAnotherHostIsRefused(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pwned"))
	}))
	t.Cleanup(evil.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/contents/") {
			http.Redirect(w, r, evil.URL+"/steal", http.StatusFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"private": false, "visibility": "public"})
	}))
	t.Cleanup(srv.Close)
	adapter := newAdapter(t, srv.URL)
	_, err := adapter.Fetch(context.Background(), "contracts/avro/README.md", pin, "")
	if err == nil || !errorsIsAllow(err) && !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("err = %v", err)
	}
}

func TestRateLimitRetriesThenStops(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if !strings.Contains(r.URL.Path, "/contents/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"private": false, "visibility": "public"})
			return
		}
		if n < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "0")
		writeContents(w, []byte("ok"))
	}))
	t.Cleanup(srv.Close)
	adapter := newAdapter(t, srv.URL)
	snap, err := adapter.Fetch(context.Background(), "contracts/avro/README.md", pin, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(snap.Content) != "ok" {
		t.Fatalf("content = %q", snap.Content)
	}
	if _, err := adapter.Fetch(context.Background(), "contracts/avro/movement-lifecycle-event.avsc", pin, ""); !errors.Is(err, github.ErrRateLimited) {
		t.Fatalf("second fetch err = %v", err)
	}
}

func TestNotModified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/contents/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"private": true, "visibility": "private"})
			return
		}
		if r.Header.Get("If-None-Match") == `"abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"abc"`)
		writeContents(w, []byte("body"))
	}))
	t.Cleanup(srv.Close)
	adapter := newAdapter(t, srv.URL)
	first, err := adapter.Fetch(context.Background(), "contracts/avro/README.md", pin, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := adapter.Fetch(context.Background(), "contracts/avro/README.md", pin, first.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if !second.NotModified || len(second.Content) != 0 || second.ACL.Private != true {
		t.Fatalf("second = %+v", second)
	}
}

func TestBaseHostOutsideAllowlist(t *testing.T) {
	adapter := github.New(github.Allowlist{
		Hosts:        []string{"api.github.com"},
		Owner:        "dipjyotimetia",
		Repo:         "distributed-systems",
		PathPrefixes: []string{"contracts/avro/"},
	}, pin)
	adapter.BaseURL = "http://169.254.169.254"
	_, err := adapter.Fetch(context.Background(), "contracts/avro/README.md", pin, "")
	if !errorsIsAllow(err) {
		t.Fatalf("err = %v", err)
	}
}

func newAdapter(t *testing.T, base string) *github.Adapter {
	t.Helper()
	u := strings.TrimPrefix(base, "http://")
	host := u
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	adapter := github.New(github.Allowlist{
		Hosts:         []string{host},
		Owner:         "dipjyotimetia",
		Repo:          "distributed-systems",
		PathPrefixes:  []string{"contracts/avro/"},
		ClassByPrefix: map[string]string{"contracts/avro/": "contract"},
	}, pin)
	adapter.BaseURL = base
	adapter.Token = "test-token"
	adapter.HTTP = &http.Client{CheckRedirect: adapter.HTTP.CheckRedirect}
	return adapter
}

func writeContents(w http.ResponseWriter, content []byte) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":     "file",
		"encoding": "base64",
		"size":     len(content),
		"content":  base64.StdEncoding.EncodeToString(content),
	})
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func errorsIsAllow(err error) bool {
	return errors.Is(err, github.ErrNotAllowlisted)
}
