package publish

import (
	"fmt"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
)

// GitHubFromConfig builds the knowledge-repository client from the loaded
// workspace. Owner and repo come from that config, which already applied
// GITHUB_KNOWLEDGE_OWNER and GITHUB_KNOWLEDGE_REPO. Callers do not read those
// variables again.
func GitHubFromConfig(cfg config.Config, lookup func(string) string) (*GitHub, error) {
	if lookup == nil {
		lookup = func(string) string { return "" }
	}
	if cfg.Workspace.KnowledgeOwner == "" || cfg.Workspace.KnowledgeRepo == "" {
		return nil, fmt.Errorf("set workspace.knowledge_owner and workspace.knowledge_repo")
	}
	client := &GitHub{
		BaseURL:      firstNonEmpty(lookup("GITHUB_API_URL"), "https://api.github.com"),
		Owner:        cfg.Workspace.KnowledgeOwner,
		Repo:         cfg.Workspace.KnowledgeRepo,
		Branch:       cfg.Workspace.DefaultBranch,
		Installation: lookup("GITHUB_APP_INSTALLATION_ID"),
	}
	if lookup("GITHUB_APP_ID") != "" {
		client.Tokens = NewAppToken(AppCredentials{
			BaseURL:        client.BaseURL,
			AppID:          lookup("GITHUB_APP_ID"),
			InstallationID: lookup("GITHUB_APP_INSTALLATION_ID"),
			PrivateKeyPEM:  lookup("GITHUB_APP_PRIVATE_KEY"),
		})
		return client, nil
	}
	client.Token = lookup("GITHUB_APP_TOKEN")
	return client, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
