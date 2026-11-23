package main

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/spf13/cobra"
)

func newServe() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the HTTP control API",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				configPath = os.Getenv("AKF_CONFIG")
			}
			cfg, err := config.Load(configPath, os.Getenv)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			db, err := postgres.Open(ctx, cfg.DatabaseURL)
			if err != nil {
				return err
			}
			defer db.Close()
			authn, err := auth.New(cfg.Tokens)
			if err != nil {
				return err
			}
			srv := api.New(nil, db, db, authn)
			srv.SetKnowledge(index.New(db))
			srv.Gaps = gaps.NewService(db)
			srv.DB = db
			srv.WebhookSecret = os.Getenv("GITHUB_WEBHOOK_SECRET")
			httpServer := &http.Server{Addr: cfg.HTTPAddr, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
			go func() {
				<-ctx.Done()
				shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = httpServer.Shutdown(shutdown)
			}()
			fmt.Fprintf(cmd.OutOrStdout(), "listening %s\n", cfg.HTTPAddr)
			if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file")
	return cmd
}

func newRetain() *cobra.Command {
	var configPath, workspace string
	cmd := &cobra.Command{
		Use:   "retain",
		Short: "Delete operational rows older than the workspace retention window",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, id, err := loadOps(configPath, workspace)
			if err != nil {
				return err
			}
			svc, _, err := openIndex(configPath, workspace)
			if err != nil {
				return err
			}
			defer svc.DB.Close()
			if err := svc.DB.EnforceRetention(context.Background(), id); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "retention applied")
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	return cmd
}

func newExport() *cobra.Command {
	var configPath, workspace string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Write gap metadata to stdout. Snapshot bodies are omitted.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, id, err := loadOps(configPath, workspace)
			if err != nil {
				return err
			}
			svc, _, err := openIndex(configPath, workspace)
			if err != nil {
				return err
			}
			defer svc.DB.Close()
			raw, err := svc.DB.Export(context.Background(), id)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(raw)
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	return cmd
}
