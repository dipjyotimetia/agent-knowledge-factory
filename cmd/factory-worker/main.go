// Command factory-worker runs the curator workflow.
// It registers TriageGapWorkflow. Activities need the database, pinned source
// evidence, and a GitHub App installation token.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/buildinfo"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/model"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Info("factory-worker", "version", buildinfo.Version)
	if len(os.Args) > 1 && os.Args[1] == "--check" {
		return
	}
	address := os.Getenv("TEMPORAL_ADDRESS")
	if address == "" {
		log.Info("TEMPORAL_ADDRESS is unset; worker is idle")
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		<-ctx.Done()
		return
	}
	cfg, err := config.Load(os.Getenv("AKF_CONFIG"), os.Getenv)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	if cfg.DatabaseURL == "" {
		log.Error("AKF_DATABASE_URL is required to run activities")
		os.Exit(1)
	}
	ctx := context.Background()
	rules, err := workflows.RulesFromConfig(cfg)
	if err != nil {
		log.Error("rules", "err", err)
		os.Exit(1)
	}
	evidence, err := workflows.EvidenceFromConfig(ctx, cfg, os.Getenv)
	if err != nil {
		log.Error("evidence", "err", err)
		os.Exit(1)
	}
	db, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("database", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	publisher, err := knowledgePublisher()
	if err != nil {
		log.Error("github app", "err", err)
		os.Exit(1)
	}
	temporalClient, err := client.Dial(client.Options{HostPort: address, Namespace: envOr("TEMPORAL_NAMESPACE", "default")})
	if err != nil {
		log.Error("temporal", "err", err)
		os.Exit(1)
	}
	defer temporalClient.Close()
	w := worker.New(temporalClient, envOr("TEMPORAL_TASK_QUEUE", "knowledge-curator"), worker.Options{})
	w.RegisterWorkflow(workflows.TriageGapWorkflow)
	calls, fetches, tokens := cfg.Curation.Limits()
	activity := &workflows.Steps{
		Workspace:        cfg.Workspace.ID,
		KnowledgeRoot:    cfg.Workspace.KnowledgeRoot,
		Gaps:             gaps.NewService(db),
		DB:               db,
		Index:            index.New(db),
		Publisher:        publisher,
		Evidence:         evidence,
		Rules:            rules,
		MaxModelCalls:    calls,
		MaxSourceFetches: fetches,
		MaxTokens:        tokens,
		CasesPath:        cfg.Curation.CasesPath,
	}
	if os.Getenv("AKF_MODEL_API_KEY") != "" {
		activity.Advisor = model.Gate{Provider: &model.OpenAI{
			BaseURL: envOr("AKF_MODEL_BASE_URL", "https://api.openai.com/v1"),
			APIKey:  os.Getenv("AKF_MODEL_API_KEY"),
			Model:   os.Getenv("AKF_MODEL_NAME"),
		}, MaxTokens: tokens}
	}
	w.RegisterActivity(activity)
	log.Info("polling", "queue", envOr("TEMPORAL_TASK_QUEUE", "knowledge-curator"), "rules", len(rules), "evidence", len(evidence))
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Error("worker", "err", err)
		os.Exit(1)
	}
}

func knowledgePublisher() (*publish.GitHub, error) {
	publisher := &publish.GitHub{
		BaseURL: envOr("GITHUB_API_URL", "https://api.github.com"),
		Owner:   os.Getenv("GITHUB_KNOWLEDGE_OWNER"),
		Repo:    os.Getenv("GITHUB_KNOWLEDGE_REPO"),
	}
	if os.Getenv("GITHUB_APP_ID") != "" {
		publisher.Tokens = publish.NewAppToken(publish.AppCredentials{
			BaseURL:        publisher.BaseURL,
			AppID:          os.Getenv("GITHUB_APP_ID"),
			InstallationID: os.Getenv("GITHUB_APP_INSTALLATION_ID"),
			PrivateKeyPEM:  os.Getenv("GITHUB_APP_PRIVATE_KEY"),
		})
		return publisher, nil
	}
	publisher.Token = os.Getenv("GITHUB_APP_TOKEN")
	return publisher, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
