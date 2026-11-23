package slack_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/ingestion/slack"
)

func TestFetchPinsARecordedMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "channel=C1") {
			t.Errorf("query %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"ok":true,"messages":[{"text":"Use eventId in the header","ts":"1700000000.000100"}]}`))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(strings.TrimPrefix(srv.URL, "https://"), "http://")
	msg, err := slack.Fetch(context.Background(), srv.URL, "token", "C1", "1700000000.000100", []string{host}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if msg.Revision != "1700000000.000100" || !strings.Contains(msg.Text, "eventId") || !strings.Contains(msg.Text, "UNTRUSTED SOURCE DATA") {
		t.Fatalf("message = %+v", msg)
	}
}
