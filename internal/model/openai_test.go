package model_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/model"
)

func TestOpenAIAdapterParsesUsage(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"decision\":\"no-change\",\"edits\":[],\"claims\":[]}"}}],"usage":{"prompt_tokens":11,"completion_tokens":4}}`))
	}))
	t.Cleanup(srv.Close)
	provider := &model.OpenAI{BaseURL: srv.URL, APIKey: "test-key", Model: "fake-model", HTTP: srv.Client()}
	req := curator.Request{
		Summary: "dedupe",
		Evidence: []curator.Evidence{{
			ID: "readme", Classification: "contract", Revision: strings.Repeat("a", 40), Digest: "sha256:x",
			Content: "deduplicate by `eventId`",
		}},
	}
	prop, err := model.Propose(context.Background(), provider, req, 200)
	if err != nil {
		t.Fatal(err)
	}
	if prop.Model != "fake-model" || prop.InputTokens != 11 || prop.OutputTokens != 4 || prop.Decision != "no-change" {
		t.Fatalf("proposal = %+v", prop)
	}
	if sawAuth != "Bearer test-key" {
		t.Fatalf("auth = %s", sawAuth)
	}
}

func TestModelCannotInventAToken(t *testing.T) {
	content := `{"decision":"update","edits":[{"path":"skills/movement-event-dedupe/SKILL.md","old":"deduplicate by ` + "`movementId`" + `","new":"deduplicate by ` + "`password`" + `"}],"claims":[{"claim":"use password","evidence_ids":["readme"]}]}`
	payload, err := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"content": content}}},
		"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	provider := &model.OpenAI{BaseURL: srv.URL, APIKey: "test-key", Model: "fake-model", HTTP: srv.Client()}
	_, err = model.Propose(context.Background(), provider, curator.Request{
		Files: map[string]string{"skills/movement-event-dedupe/SKILL.md": "deduplicate by `movementId`"},
		Evidence: []curator.Evidence{{
			ID: "readme", Classification: "contract", Location: "readme", Revision: strings.Repeat("a", 40), Digest: "sha256:x",
			Content: "deduplicate by `eventId`",
		}},
	}, 200)
	if err == nil {
		t.Fatal("ungrounded model edit was accepted")
	}
}
