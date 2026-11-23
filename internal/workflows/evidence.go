package workflows

import (
	"context"
	"fmt"
	"strings"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/ingestion/github"
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
		if src.Provider != "" && src.Provider != "github" {
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
				ID:             path,
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
