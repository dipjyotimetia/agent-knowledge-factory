package workflows_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
)

func TestNotionSourceIsAllowlistedAndPinned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"page-1","last_edited_time":"2026-11-01T00:00:00Z","properties":{"title":{"title":[{"plain_text":"Notes"}]}},"plain_text":"Do not copy rows."}`))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(strings.TrimPrefix(srv.URL, "https://"), "http://")
	got, err := workflows.EvidenceFromConfig(context.Background(), config.Config{Sources: []config.Source{{
		ID: "notes", Provider: "notion", BaseURL: srv.URL, Paths: []string{"page-1"}, AllowHosts: []string{host},
	}}}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Revision != "2026-11-01T00:00:00Z" || got[0].Classification != "secondary" {
		t.Fatalf("evidence = %+v", got)
	}
}
