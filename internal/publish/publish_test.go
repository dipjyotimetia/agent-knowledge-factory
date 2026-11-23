package publish_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

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
		case r.Method == http.MethodPut:
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

func jsonNew(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}
