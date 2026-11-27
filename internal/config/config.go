// Package config loads process configuration. Secret values are read from the
// environment at startup. Files in Git hold references, never secret material.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Token binds a bearer credential to one workspace and one role.
// Token material is supplied either inline for tests or through TokenEnv.
type Token struct {
	Token       string `yaml:"token"`
	TokenEnv    string `yaml:"token_env"`
	WorkspaceID string `yaml:"workspace_id"`
	Role        string `yaml:"role"`
}

// Workspace is the configured knowledge boundary.
// KnowledgeOwner and KnowledgeRepo are the publisher's repository.
// A knowledge_repo value of "owner/repo" fills both when knowledge_owner is empty.
// DefaultBranch is the protected branch the publisher and the webhook observe.
type Workspace struct {
	ID             string `yaml:"workspace_id"`
	DisplayName    string `yaml:"display_name"`
	KnowledgeOwner string `yaml:"knowledge_owner"`
	KnowledgeRepo  string `yaml:"knowledge_repo"`
	KnowledgeRoot  string `yaml:"knowledge_root"`
	DefaultBranch  string `yaml:"default_branch"`
	RetentionDays  int    `yaml:"retention_days"`
}

// Source is a read-only evidence repository. CredentialRef is an env reference,
// never the secret itself.
type Source struct {
	ID             string            `yaml:"id"`
	Provider       string            `yaml:"provider"`
	Owner          string            `yaml:"owner"`
	Repo           string            `yaml:"repo"`
	PinSHA         string            `yaml:"pin_sha"`
	CredentialRef  string            `yaml:"credential_ref"`
	AllowHosts     []string          `yaml:"allow_hosts"`
	BaseURL        string            `yaml:"base_url"`
	Channel        string            `yaml:"channel"`
	AllowPaths     []string          `yaml:"allow_paths"`
	Paths          []string          `yaml:"paths"`
	Classification map[string]string `yaml:"classification"`
}

// Curation names the deterministic rules and the budgets the worker enforces.
type Curation struct {
	Rules             []string `yaml:"rules"`
	MaxModelCalls     int      `yaml:"max_model_calls"`
	MaxSourceFetches  int      `yaml:"max_source_fetches"`
	MaxTokens         int      `yaml:"max_tokens"`
	CasesPath         string   `yaml:"cases"`
	MaxInvestigations int      `yaml:"max_investigations"`
	ReviewReminder    string   `yaml:"review_reminder"`
	ReviewExpiry      string   `yaml:"review_expiry"`
}

// Limits returns the budgets, using the banking defaults when a value is unset.
func (c Curation) Limits() (calls, fetches, tokens int) {
	calls, fetches, tokens = c.MaxModelCalls, c.MaxSourceFetches, c.MaxTokens
	if calls <= 0 {
		calls = 4
	}
	if fetches <= 0 {
		fetches = 20
	}
	if tokens <= 0 {
		tokens = 8000
	}
	return calls, fetches, tokens
}

// ReviewWindows returns the review reminder and parking deadlines.
func (c Curation) ReviewWindows() (time.Duration, time.Duration, error) {
	reminder, expiry := 7*24*time.Hour, 30*24*time.Hour
	var err error
	if c.ReviewReminder != "" {
		reminder, err = time.ParseDuration(c.ReviewReminder)
		if err != nil {
			return 0, 0, err
		}
	}
	if c.ReviewExpiry != "" {
		expiry, err = time.ParseDuration(c.ReviewExpiry)
		if err != nil {
			return 0, 0, err
		}
	}
	if reminder <= 0 || expiry <= reminder || c.MaxInvestigations < 0 {
		return 0, 0, fmt.Errorf("invalid investigation limits or review windows")
	}
	return reminder, expiry, nil
}

// Config is the resolved process configuration.
type Config struct {
	HTTPAddr             string
	DatabaseURL          string
	InstallerDatabaseURL string
	Dev                  bool
	Tokens               []Token
	Workspace            Workspace
	Sources              []Source
	Curation             Curation
	ConfigPath           string
}

type fileConfig struct {
	HTTPAddr             string    `yaml:"http_addr"`
	DatabaseURL          string    `yaml:"database_url"`
	InstallerDatabaseURL string    `yaml:"installer_database_url"`
	Tokens               []Token   `yaml:"tokens"`
	DevTokens            []Token   `yaml:"dev_tokens"`
	Workspace            Workspace `yaml:"workspace"`
	Sources              []Source  `yaml:"sources"`
	Curation             Curation  `yaml:"curation"`
}

// Lookup reads an environment variable. Tests replace it.
type Lookup func(string) string

// Load reads an optional YAML file and applies environment overrides.
// DevTokens are ignored unless AKF_DEV is 1, true, or yes.
func Load(path string, lookup Lookup) (Config, error) {
	if lookup == nil {
		lookup = os.Getenv
	}
	var file fileConfig
	if path != "" {
		raw, err := os.ReadFile(path) // #nosec G304 -- path is the operator config file, not request input
		if err != nil {
			return Config{}, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(raw, &file); err != nil {
			return Config{}, fmt.Errorf("parse config: %w", err)
		}
	}
	workspace, err := resolveWorkspace(file.Workspace, lookup)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		HTTPAddr:             firstNonEmpty(lookup("AKF_HTTP_ADDR"), file.HTTPAddr, ":8080"),
		DatabaseURL:          firstNonEmpty(lookup("AKF_DATABASE_URL"), file.DatabaseURL),
		InstallerDatabaseURL: firstNonEmpty(lookup("AKF_INSTALLER_DATABASE_URL"), file.InstallerDatabaseURL),
		Dev:                  envTruthy(lookup("AKF_DEV")),
		Workspace:            workspace,
		Sources:              file.Sources,
		Curation:             file.Curation,
		ConfigPath:           path,
	}
	tokens, err := resolveTokens(file.Tokens, lookup)
	if err != nil {
		return Config{}, err
	}
	if cfg.Dev {
		devTokens, err := resolveTokens(file.DevTokens, lookup)
		if err != nil {
			return Config{}, err
		}
		tokens = append(tokens, devTokens...)
	}
	cfg.Tokens = tokens
	return cfg, nil
}

func resolveTokens(in []Token, lookup Lookup) ([]Token, error) {
	out := make([]Token, 0, len(in))
	for _, token := range in {
		resolved := token
		if token.TokenEnv != "" {
			resolved.Token = lookup(token.TokenEnv)
		}
		if resolved.Token == "" {
			continue
		}
		switch resolved.Role {
		case "agent", "curator", "reviewer":
		default:
			return nil, fmt.Errorf("token role %q is not agent, curator, or reviewer", resolved.Role)
		}
		if strings.Contains(resolved.Token, "\n") || strings.Contains(resolved.Token, "\x00") {
			return nil, fmt.Errorf("token for workspace %s contains a control character", resolved.WorkspaceID)
		}
		out = append(out, resolved)
	}
	return out, nil
}

func resolveWorkspace(ws Workspace, lookup Lookup) (Workspace, error) {
	if owner := lookup("GITHUB_KNOWLEDGE_OWNER"); owner != "" {
		ws.KnowledgeOwner = owner
	}
	if repo := lookup("GITHUB_KNOWLEDGE_REPO"); repo != "" {
		ws.KnowledgeRepo = repo
	}
	if root := lookup("AKF_KNOWLEDGE_ROOT"); root != "" {
		ws.KnowledgeRoot = root
	}
	if branch := lookup("AKF_DEFAULT_BRANCH"); branch != "" {
		ws.DefaultBranch = branch
	}
	if ws.KnowledgeOwner == "" {
		if owner, repo, ok := strings.Cut(ws.KnowledgeRepo, "/"); ok && owner != "" && repo != "" && !strings.Contains(repo, "/") {
			ws.KnowledgeOwner = owner
			ws.KnowledgeRepo = repo
		}
	}
	if ws.DefaultBranch == "" {
		ws.DefaultBranch = "main"
	}
	if err := validBranch(ws.DefaultBranch); err != nil {
		return Workspace{}, err
	}
	return ws, nil
}

func validBranch(branch string) error {
	if branch == "" || strings.Contains(branch, "..") || strings.ContainsAny(branch, " \t\\") || strings.HasPrefix(branch, "/") {
		return fmt.Errorf("invalid default branch %q", branch)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func envTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
