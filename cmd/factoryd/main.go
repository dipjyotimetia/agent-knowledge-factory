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
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/migrations"
)

func main() {
	configPath := flag.String("config", os.Getenv("AKF_CONFIG"), "path to a YAML config file")
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
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.New(log, ready, reader, authn).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Info("listening", "addr", cfg.HTTPAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}
