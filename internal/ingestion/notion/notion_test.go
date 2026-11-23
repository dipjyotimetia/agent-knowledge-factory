package notion_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/ingestion/notion"
)

func TestFetchPinsARecordedPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pages/page-1" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"page-1","last_edited_time":"2026-11-01T00:00:00Z","properties":{"title":{"title":[{"plain_text":"Movement notes"}]}},"plain_text":"Query the source. Do not copy rows."}`))
	}))
	defer srv.Close()
	page, err := notion.Fetch(context.Background(), srv.URL, "token", "page-1", []string{mustHost(srv.URL)}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if page.Revision != "2026-11-01T00:00:00Z" || !strings.Contains(page.Body, "Do not copy rows") || !strings.Contains(page.Body, "UNTRUSTED SOURCE DATA") {
		t.Fatalf("page = %+v", page)
	}
	if _, err := notion.Fetch(context.Background(), srv.URL, "token", "page-1", []string{"example.invalid"}, srv.Client()); err == nil {
		t.Fatal("expected a host outside the allowlist to fail")
	}
}

func mustHost(raw string) string {
	return strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
}
