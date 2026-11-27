// Command factoryd serves the control API.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/api"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/auth"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/mcpserver"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/runtime"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
	"github.com/dipjyotimetia/agent-knowledge-factory/migrations"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	configPath := flag.String("config", os.Getenv("AKF_CONFIG"), "path to a YAML config file")
	mcpMode := flag.Bool("mcp", false, "also serve the knowledge MCP server on stdio")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := config.Load(*configPath, os.Getenv)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	if cfg.Dev {
		log.Warn("AKF_DEV is enabled; development tokens are accepted and must not be used in production")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var appDB *postgres.DB
	if cfg.InstallerDatabaseURL != "" {
		installer, openErr := postgres.Open(ctx, cfg.InstallerDatabaseURL)
		if openErr != nil {
			log.Error("installer database", "err", openErr)
			os.Exit(1)
		}
		installer.Installer = true
		defer installer.Close()
		if migrateErr := installer.Migrate(ctx, migrations.Files); migrateErr != nil {
			log.Error("migrate", "err", migrateErr)
			os.Exit(1)
		}
		if grantErr := installer.GrantAppRole(ctx, "akf_app"); grantErr != nil {
			log.Error("grant", "err", grantErr)
			os.Exit(1)
		}
	}
	if cfg.DatabaseURL != "" {
		var openErr error
		appDB, openErr = postgres.Open(ctx, cfg.DatabaseURL)
		if openErr != nil {
			log.Error("database", "err", openErr)
			os.Exit(1)
		}
		defer appDB.Close()
		if cfg.InstallerDatabaseURL == "" {
			log.Warn("serving with the same database role used for migrations; set AKF_INSTALLER_DATABASE_URL so the API role is not a superuser")
			appDB.Installer = true
			if migrateErr := appDB.Migrate(ctx, migrations.Files); migrateErr != nil {
				log.Error("migrate", "err", migrateErr)
				os.Exit(1)
			}
			appDB.Installer = false
		}
	}

	authn, err := auth.New(cfg.Tokens)
	if err != nil {
		log.Error("auth", "err", err)
		os.Exit(1)
	}
	var reader api.WorkspaceReader
	var ready api.ReadyCheck
	if appDB != nil {
		reader = appDB
		ready = appDB
	}
	var knowledge *index.Service
	if appDB != nil {
		knowledge = index.New(appDB)
		knowledge.Hybrid = os.Getenv("SEARCH_MODE") == "hybrid"
	}
	apiServer := api.New(log, ready, reader, authn)
	if knowledge != nil {
		apiServer.SetKnowledge(knowledge)
	}
	apiServer.WebhookSecret = os.Getenv("GITHUB_WEBHOOK_SECRET")
	apiServer.DefaultBranch = cfg.Workspace.DefaultBranch
	if appDB != nil && cfg.Workspace.ID != "" {
		apiServer.Publication, _ = runtime.NewPublication(cfg, appDB, knowledge, os.Getenv)
		apiServer.Gaps = gaps.NewService(appDB)
		apiServer.DB = appDB
	}
	var startFn func(context.Context, string) error
	temporalCfg := workflows.TemporalFromLookup(os.Getenv)
	if temporalCfg.Address != "" {
		starter, temporalClient, err := workflows.DialStarter(temporalCfg, os.Getenv("TEMPORAL_TASK_QUEUE"), cfg.Workspace.ID)
		if err != nil {
			log.Error("temporal", "err", err)
			os.Exit(1)
		}
		defer temporalClient.Close()
		startFn = starter.StartInvestigation
		if appDB != nil {
			apiServer.Reviews = runtime.NewReviews(temporalClient, cfg, appDB, knowledge, os.Getenv)
		}
	}
	if *mcpMode {
		if cfg.Workspace.ID == "" {
			log.Error("mcp requires workspace.workspace_id in config")
			os.Exit(1)
		}
		var mcpReader mcpserver.Reader
		if knowledge != nil {
			mcpReader = knowledge
		}
		var reporter mcpserver.Reporter = gaps.NewMemory()
		if appDB != nil {
			reporter = gaps.NewService(appDB)
		}
		mcpServer := mcpserver.New(mcpserver.Deps{
			WorkspaceID: cfg.Workspace.ID,
			AgentID:     "stdio",
			Reader:      mcpReader,
			Gaps:        reporter,
			Start:       startFn,
		})
		go serveHTTP(ctx, log, cfg.HTTPAddr, apiServer.Handler())
		if err := mcpServer.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
			log.Error("mcp", "err", err)
			os.Exit(1)
		}
		return
	}
	serveHTTP(ctx, log, cfg.HTTPAddr, apiServer.Handler())
}

func serveHTTP(ctx context.Context, log *slog.Logger, addr string, handler http.Handler) {
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { // #nosec G118 -- the process context is already cancelled; shutdown needs its own deadline
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Info("listening", "addr", addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}
