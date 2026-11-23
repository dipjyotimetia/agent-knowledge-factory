package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDevTokensRequireExplicitDevMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := []byte(`
http_addr: ":9090"
workspace:
  workspace_id: banking-demo
  display_name: Banking demo
  knowledge_repo: knowledge-factory-demo-knowledge
tokens:
  - token_env: AKF_AGENT_TOKEN
    workspace_id: banking-demo
    role: agent
dev_tokens:
  - token: dev-agent-banking
    workspace_id: banking-demo
    role: agent
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	lookup := func(key string) string {
		switch key {
		case "AKF_AGENT_TOKEN":
			return "from-env"
		default:
			return ""
		}
	}
	cfg, err := Load(path, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Dev {
		t.Fatal("dev mode enabled without AKF_DEV")
	}
	if len(cfg.Tokens) != 1 || cfg.Tokens[0].Token != "from-env" || cfg.Tokens[0].Role != "agent" {
		t.Fatalf("tokens = %+v", cfg.Tokens)
	}
	lookupDev := func(key string) string {
		if key == "AKF_DEV" {
			return "1"
		}
		return lookup(key)
	}
	cfg, err = Load(path, lookupDev)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Tokens) != 2 {
		t.Fatalf("dev tokens not loaded: %+v", cfg.Tokens)
	}
}

func TestLoadRejectsUnknownRole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("tokens:\n  - token: abc\n    workspace_id: banking-demo\n    role: merge\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, func(string) string { return "" }); err == nil {
		t.Fatal("expected unknown role to fail")
	}
}
