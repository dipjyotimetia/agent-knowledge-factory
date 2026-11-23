// Package config loads process configuration. Secret values are read from the
// environment at startup. Files in Git hold references, never secret material.
package config

import (
	"fmt"
	"os"
	"strings"

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
type Workspace struct {
	ID            string `yaml:"workspace_id"`
	DisplayName   string `yaml:"display_name"`
	KnowledgeRepo string `yaml:"knowledge_repo"`
	KnowledgeRoot string `yaml:"knowledge_root"`
	RetentionDays int    `yaml:"retention_days"`
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
	AllowPaths     []string          `yaml:"allow_paths"`
	Paths          []string          `yaml:"paths"`
	Classification map[string]string `yaml:"classification"`
}

// Curation names the deterministic rules the worker applies.
type Curation struct {
	Rules []string `yaml:"rules"`
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
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(raw, &file); err != nil {
			return Config{}, fmt.Errorf("parse config: %w", err)
		}
	}
	cfg := Config{
		HTTPAddr:             firstNonEmpty(lookup("AKF_HTTP_ADDR"), file.HTTPAddr, ":8080"),
		DatabaseURL:          firstNonEmpty(lookup("AKF_DATABASE_URL"), file.DatabaseURL),
		InstallerDatabaseURL: firstNonEmpty(lookup("AKF_INSTALLER_DATABASE_URL"), file.InstallerDatabaseURL),
		Dev:                  envTruthy(lookup("AKF_DEV")),
		Workspace:            file.Workspace,
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
