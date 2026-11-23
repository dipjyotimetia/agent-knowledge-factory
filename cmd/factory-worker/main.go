// Command factory-worker runs the curator workflow.
// It registers TriageGapWorkflow. Activities need the database and a publisher,
// which are wired when AKF_DATABASE_URL and TEMPORAL_ADDRESS are set.
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
	db, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("database", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	temporalClient, err := client.Dial(client.Options{HostPort: address, Namespace: envOr("TEMPORAL_NAMESPACE", "default")})
	if err != nil {
		log.Error("temporal", "err", err)
		os.Exit(1)
	}
	defer temporalClient.Close()
	w := worker.New(temporalClient, envOr("TEMPORAL_TASK_QUEUE", "knowledge-curator"), worker.Options{})
	w.RegisterWorkflow(workflows.TriageGapWorkflow)
	w.RegisterActivity(&workflows.Steps{
		Workspace:     cfg.Workspace.ID,
		KnowledgeRoot: cfg.Workspace.KnowledgeRoot,
		Gaps:          gaps.NewService(db),
		DB:            db,
		Index:         index.New(db),
		Publisher: &publish.GitHub{
			BaseURL: envOr("GITHUB_API_URL", "https://api.github.com"),
			Owner:   os.Getenv("GITHUB_KNOWLEDGE_OWNER"),
			Repo:    os.Getenv("GITHUB_KNOWLEDGE_REPO"),
			Token:   os.Getenv("GITHUB_APP_TOKEN"),
		},
	})
	log.Info("polling", "queue", envOr("TEMPORAL_TASK_QUEUE", "knowledge-curator"))
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Error("worker", "err", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
