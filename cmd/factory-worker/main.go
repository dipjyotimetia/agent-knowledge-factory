// Command factory-worker runs the curator workflow.
// Its activities need the database, pinned source
// evidence, and a GitHub App installation token.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/buildinfo"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/model"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publication"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/runtime"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
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
	reminder, park, err := cfg.Curation.ReviewWindows()
	if err != nil {
		log.Error("review windows", "err", err)
		os.Exit(1)
	}

	db, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("database", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	knowledge := hybridIndex(db)
	releases, err := runtime.NewPublication(cfg, db, knowledge, os.Getenv)
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
	queue := workflows.AttemptQueue(os.Getenv("TEMPORAL_TASK_QUEUE"), cfg.Workspace.ID)
	w := worker.New(temporalClient, queue, worker.Options{})
	w.RegisterWorkflow(workflows.InvestigateGapWorkflowV2)
	w.RegisterWorkflow(workflows.DrainInvestigationsWorkflowV2)
	w.RegisterWorkflow(workflows.RetainWorkspaceWorkflowV2)
	attempts := runtime.NewInvestigationStore(cfg, db)
	starter := workflows.AttemptStarter{Client: temporalClient, Queue: queue, Store: attempts}
	if err := ensureCurationSchedule(ctx, temporalClient, queue, cfg.Workspace.ID); err != nil {
		log.Error("curation schedule", "err", err)
	}

	calls, fetches, tokens := cfg.Curation.Limits()
	steps := workflows.Steps{
		Workspace:        cfg.Workspace.ID,
		KnowledgeRoot:    cfg.Workspace.KnowledgeRoot,
		Gaps:             gaps.NewService(db),
		DB:               db,
		Index:            knowledge,
		Publication:      releases,
		Publisher:        releases.Forge,
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
		steps.Advisor = model.Gate{Provider: &model.OpenAI{
			BaseURL: envOr("AKF_MODEL_BASE_URL", "https://api.openai.com/v1"),
			APIKey:  os.Getenv("AKF_MODEL_API_KEY"),
			Model:   os.Getenv("AKF_MODEL_NAME"),
		}, MaxTokens: tokens}
	}
	w.RegisterActivity(&workflows.InvestigationActivities{Store: attempts, Steps: steps, Starter: starter, Reminder: reminder, ParkAfter: park, PauseIntake: os.Getenv("AKF_PAUSE_INTAKE") == "1"})
	workerCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go pollPublications(workerCtx, log, releases)
	if os.Getenv("AKF_PAUSE_INTAKE") != "1" {
		go pollInvestigations(workerCtx, log, starter)
	}
	log.Info("polling", "queue", queue, "rules", len(rules))
	go enqueueReconciliation(workerCtx, log, releases)

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
	if os.Getenv("AKF_PAUSE_INTAKE") != "1" {
		if err := ensureSchedule(ctx, c, queue, "akf-curation-v2-"+workspace, workflows.DrainInvestigationsWorkflowV2, every,
			workflows.DrainInvestigationsInput{WorkspaceID: workspace}); err != nil {
			return err
		}
	}
	return ensureSchedule(ctx, c, queue, "akf-retention-v2-"+workspace, workflows.RetainWorkspaceWorkflowV2, 24*time.Hour, workspace)
}

func ensureSchedule(ctx context.Context, c client.Client, queue, id string, fn any, every time.Duration, arg any) error {
	_, err := c.ScheduleClient().Create(ctx, client.ScheduleOptions{
		ID: id, Overlap: enums.SCHEDULE_OVERLAP_POLICY_SKIP,
		Spec:   client.ScheduleSpec{Intervals: []client.ScheduleIntervalSpec{{Every: every}}},
		Action: &client.ScheduleWorkflowAction{ID: id, Workflow: fn, TaskQueue: queue, Args: []any{arg}},
	})
	var existing *serviceerror.AlreadyExists
	if errors.As(err, &existing) || errors.Is(err, temporal.ErrScheduleAlreadyRunning) {
		return nil
	}
	return err
}

func pollInvestigations(ctx context.Context, log *slog.Logger, starter workflows.AttemptStarter) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := starter.Dispatch(run)
			cancel()
			if err != nil {
				log.Warn("attempt dispatch incomplete")
			}
		}
	}
}

func enqueueReconciliation(ctx context.Context, log *slog.Logger, service *publication.Service) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := service.RequestRelease(run, publication.Request{Trigger: "reconcile"})
			cancel()
			if err != nil {
				log.Warn("publication reconciliation enqueue incomplete")
			}
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func pollPublications(ctx context.Context, log *slog.Logger, service *publication.Service) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run, cancel := context.WithTimeout(ctx, 4*time.Minute)
			_, err := service.Process(run)
			cancel()
			if err != nil && !errors.Is(err, publication.ErrHeld) && !errors.Is(err, context.Canceled) {
				log.Warn("publication operation incomplete", "err", err)
			}
		}
	}
}
