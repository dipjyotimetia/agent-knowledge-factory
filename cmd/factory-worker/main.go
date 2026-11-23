// Command factory-worker runs the curator workflow.
// It registers TriageGapWorkflow. Activities need the database, pinned source
// evidence, and a GitHub App installation token.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/buildinfo"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
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
	publisher, err := publish.GitHubFromConfig(cfg, os.Getenv)
	if err != nil {
		log.Error("github app", "err", err)
		os.Exit(1)
	}
	temporalClient, err := workflows.Dial(workflows.TemporalFromLookup(os.Getenv))
	if err != nil {
		log.Error("temporal", "err", err)
		os.Exit(1)
	}
	defer temporalClient.Close()
	w := worker.New(temporalClient, envOr("TEMPORAL_TASK_QUEUE", "knowledge-curator"), worker.Options{})
	w.RegisterWorkflow(workflows.TriageGapWorkflow)
	w.RegisterWorkflow(workflows.DrainOpenWorkflow)
	if err := ensureCurationSchedule(context.Background(), temporalClient, envOr("TEMPORAL_TASK_QUEUE", "knowledge-curator"), cfg.Workspace.ID); err != nil {
		log.Error("curation schedule", "err", err)
	}
	calls, fetches, tokens := cfg.Curation.Limits()
	activity := &workflows.Steps{
		Workspace:        cfg.Workspace.ID,
		KnowledgeRoot:    cfg.Workspace.KnowledgeRoot,
		Gaps:             gaps.NewService(db),
		DB:               db,
		Index:            hybridIndex(db),
		Publisher:        publisher,
		Evidence:         evidence,
		Rules:            rules,
		MaxModelCalls:    calls,
		MaxSourceFetches: fetches,
		MaxTokens:        tokens,
		CasesPath:        cfg.Curation.CasesPath,
		FetchEvidence: func(ctx context.Context) ([]curator.Evidence, error) {
			return workflows.EvidenceFromConfig(ctx, cfg, os.Getenv)
		},
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
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if err := activity.Reconcile(context.Background()); err != nil {
				log.Error("reconcile", "err", err)
			}
			if err := db.EnforceRetention(context.Background(), cfg.Workspace.ID); err != nil {
				log.Error("retention", "err", err)
			}
		}
	}()
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Error("worker", "err", err)
		os.Exit(1)
	}
}

func hybridIndex(db *postgres.DB) *index.Service {
	svc := index.New(db)
	svc.Hybrid = os.Getenv("SEARCH_MODE") == "hybrid"
	return svc
}

func ensureCurationSchedule(ctx context.Context, c client.Client, queue, workspace string) error {
	every := 24 * time.Hour
	if raw := os.Getenv("CURATION_INTERVAL"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		every = parsed
	}
	_, err := c.ScheduleClient().Create(ctx, client.ScheduleOptions{
		ID: "akf-curation-" + workspace,
		Spec: client.ScheduleSpec{Intervals: []client.ScheduleIntervalSpec{{
			Every: every,
		}}},
		Action: &client.ScheduleWorkflowAction{
			ID:        "akf-curation-drain",
			Workflow:  workflows.DrainOpenWorkflow,
			TaskQueue: queue,
		},
	})
	if err != nil && !strings.Contains(err.Error(), "already") {
		return err
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
