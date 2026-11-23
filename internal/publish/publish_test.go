package publish_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
)

func TestPublisherCannotMerge(t *testing.T) {
	typ := reflect.TypeOf((*publish.Publisher)(nil)).Elem()
	for i := 0; i < typ.NumMethod(); i++ {
		if strings.Contains(strings.ToLower(typ.Method(i).Name), "merge") {
			t.Fatalf("publisher method %s can merge", typ.Method(i).Name)
		}
	}
	repo := publish.NewMemory("old", map[string]string{"a.md": "old"})
	var publisher publish.Publisher = repo
	result, err := publisher.CreatePullRequest(context.Background(), publish.Request{
		Title: "update", Body: "evidence", Branch: "curator/gap", Files: map[string]string{"a.md": "new"},
	})
	if err != nil {
		t.Fatal(err)
	}
	head, err := publisher.DefaultHead(context.Background())
	if err != nil || head != "old" {
		t.Fatalf("default head changed before a human merge: %s %v", head, err)
	}
	merged, err := repo.HumanMerge(result.Number)
	if err != nil {
		t.Fatal(err)
	}
	head, err = publisher.DefaultHead(context.Background())
	if err != nil || head != merged || head == "old" {
		t.Fatalf("head after human merge = %s", head)
	}
}

func TestGitHubClientRefusesMerge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "merge") {
			t.Errorf("merge path was called: %s", r.URL.Path)
		}
		switch {
		case strings.Contains(r.URL.Path, "/git/ref/"):
			_ = jsonNew(w, map[string]any{"object": map[string]any{"sha": "abc"}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/refs"):
			w.WriteHeader(http.StatusCreated)
			_ = jsonNew(w, map[string]any{})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/contents/"):
			_ = jsonNew(w, map[string]any{"sha": "blob-sha", "type": "file"})
		case r.Method == http.MethodPut:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("put body: %v", err)
			}
			if body["sha"] != "blob-sha" {
				t.Errorf("update omitted the blob sha: %#v", body["sha"])
			}
			_ = jsonNew(w, map[string]any{})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			_ = jsonNew(w, map[string]any{"number": 7, "html_url": "https://example.invalid/pull/7", "head": map[string]any{"sha": "def"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := &publish.GitHub{BaseURL: srv.URL, Owner: "acme", Repo: "knowledge", Token: "token", HTTP: srv.Client()}
	result, err := client.CreatePullRequest(context.Background(), publish.Request{
		Title: "t", Body: "b", Branch: "curator/1", Files: map[string]string{"skills/a/SKILL.md": "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Number != 7 || result.HeadSHA != "def" {
		t.Fatalf("result = %+v", result)
	}
	if _, err := client.DefaultHead(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultBranchIsConfigurable(t *testing.T) {
	var sawRef, sawBase bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/knowledge/git/ref/heads/trunk":
			sawRef = true
			_ = jsonNew(w, map[string]any{"object": map[string]any{"sha": "abc"}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/refs"):
			w.WriteHeader(http.StatusCreated)
			_ = jsonNew(w, map[string]any{})
		case r.Method == http.MethodPut:
			_ = jsonNew(w, map[string]any{})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("pull body: %v", err)
			}
			if body["base"] == "trunk" {
				sawBase = true
			}
			_ = jsonNew(w, map[string]any{"number": 3, "html_url": "https://example.invalid/pull/3", "head": map[string]any{"sha": "def"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := &publish.GitHub{BaseURL: srv.URL, Owner: "acme", Repo: "knowledge", Branch: "trunk", Token: "token", HTTP: srv.Client()}
	if _, err := client.CreatePullRequest(context.Background(), publish.Request{
		Title: "t", Body: "b", Branch: "curator/1", Files: map[string]string{"index.md": "hello"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DefaultHead(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sawRef || !sawBase {
		t.Fatalf("ref=%v base=%v", sawRef, sawBase)
	}
	client.Branch = "../main"
	if _, err := client.DefaultHead(context.Background()); err == nil {
		t.Fatal("expected an invalid branch to fail")
	}
}

func TestPublisherUsesWorkspaceRepository(t *testing.T) {
	cfg := configForPublish("acme", "knowledge", "trunk")
	client, err := publish.GitHubFromConfig(cfg, func(key string) string {
		if key == "GITHUB_API_URL" {
			return "https://example.invalid"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.Owner != "acme" || client.Repo != "knowledge" || client.Branch != "trunk" || client.BaseURL != "https://example.invalid" {
		t.Fatalf("client = %+v", client)
	}
	if _, err := publish.GitHubFromConfig(configForPublish("", "knowledge", "main"), func(string) string { return "" }); err == nil {
		t.Fatal("expected a missing owner to fail")
	}
}

func configForPublish(owner, repo, branch string) config.Config {
	return config.Config{Workspace: config.Workspace{KnowledgeOwner: owner, KnowledgeRepo: repo, DefaultBranch: branch}}
}

func jsonNew(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}
