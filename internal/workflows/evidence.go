package workflows

import (
	"context"
	"fmt"
	"strings"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/ingestion/github"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/ingestion/notion"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/ingestion/slack"
)

// RulesFromConfig resolves curation.rules. A missing list uses the banking
// dedupe rule so a worker is not started with an empty rule set.
func RulesFromConfig(cfg config.Config) ([]curator.Rule, error) {
	names := cfg.Curation.Rules
	if names == nil {
		names = []string{"movement-dedupe-key"}
	}
	return curator.RulesByName(names)
}

// EvidenceFromConfig fetches each configured source path at its pinned SHA.
func EvidenceFromConfig(ctx context.Context, cfg config.Config, lookup func(string) string) ([]curator.Evidence, error) {
	if lookup == nil {
		lookup = func(string) string { return "" }
	}
	var out []curator.Evidence
	for _, src := range cfg.Sources {
		switch src.Provider {
		case "notion":
			if len(src.Paths) == 0 {
				return nil, fmt.Errorf("notion source %s needs a page id in paths", src.ID)
			}
			token := ""
			if strings.HasPrefix(src.CredentialRef, "env:") {
				token = lookup(strings.TrimPrefix(src.CredentialRef, "env:"))
			}
			page, err := notion.Fetch(ctx, src.BaseURL, token, src.Paths[0], src.AllowHosts, nil)
			if err != nil {
				return nil, fmt.Errorf("notion %s: %w", src.ID, err)
			}
			out = append(out, curator.Evidence{
				ID: src.ID, Location: "notion://" + page.ID, Revision: page.Revision,
				Digest: page.Revision, Classification: "secondary", Content: page.Body,
			})
			continue
		case "slack":
			if src.Channel == "" || len(src.Paths) == 0 {
				return nil, fmt.Errorf("slack source %s needs channel and a timestamp path", src.ID)
			}
			token := ""
			if strings.HasPrefix(src.CredentialRef, "env:") {
				token = lookup(strings.TrimPrefix(src.CredentialRef, "env:"))
			}
			msg, err := slack.Fetch(ctx, src.BaseURL, token, src.Channel, src.Paths[0], src.AllowHosts, nil)
			if err != nil {
				return nil, fmt.Errorf("slack %s: %w", src.ID, err)
			}
			out = append(out, curator.Evidence{
				ID: src.ID, Location: "slack://" + msg.Channel + "/" + msg.TS, Revision: msg.Revision,
				Digest: msg.Revision, Classification: "secondary", Content: msg.Text,
			})
			continue
		case "", "github":
		default:
			return nil, fmt.Errorf("unsupported source provider %q", src.Provider)
		}
		adapter := github.New(github.Allowlist{
			Hosts:         src.AllowHosts,
			Owner:         src.Owner,
			Repo:          src.Repo,
			PathPrefixes:  src.AllowPaths,
			ClassByPrefix: src.Classification,
		}, src.PinSHA)
		if strings.HasPrefix(src.CredentialRef, "env:") {
			adapter.Token = lookup(strings.TrimPrefix(src.CredentialRef, "env:"))
		}
		for _, path := range src.Paths {
			snap, err := adapter.Fetch(ctx, path, src.PinSHA, "")
			if err != nil {
				return nil, fmt.Errorf("fetch %s: %w", path, err)
			}
			out = append(out, curator.Evidence{
				ID:             snap.Location + "@" + snap.Revision,
				Location:       snap.Location,
				Revision:       snap.Revision,
				Digest:         snap.Digest,
				Classification: snap.Classification,
				Content:        string(snap.Content),
			})
		}
	}
	_, fetches, _ := cfg.Curation.Limits()
	if len(out) > fetches {
		return nil, fmt.Errorf("source fetch budget exceeded: %d > %d", len(out), fetches)
	}
	return out, nil
}
