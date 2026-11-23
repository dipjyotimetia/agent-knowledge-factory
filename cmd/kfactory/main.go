// Command kfactory is the operator CLI.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/buildinfo"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/migrations"
	"github.com/spf13/cobra"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "kfactory",
		Short: "Operate the Agent Knowledge Factory",
	}
	root.AddCommand(newVersion(), newInit())
	return root
}

func newVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the factory version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), buildinfo.Version)
		},
	}
}

func newInit() *cobra.Command {
	var configPath string
	var workspaceID string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a workspace using the installer database connection",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				configPath = os.Getenv("AKF_CONFIG")
			}
			cfg, err := config.Load(configPath, os.Getenv)
			if err != nil {
				return err
			}
			id := workspaceID
			if id == "" {
				id = cfg.Workspace.ID
			}
			if err := domain.ValidateWorkspaceID(id); err != nil {
				return err
			}
			dsn := cfg.InstallerDatabaseURL
			if dsn == "" {
				dsn = cfg.DatabaseURL
			}
			if dsn == "" {
				return fmt.Errorf("set AKF_INSTALLER_DATABASE_URL")
			}
			ctx := context.Background()
			db, err := postgres.Open(ctx, dsn)
			if err != nil {
				return err
			}
			defer db.Close()
			db.Installer = true
			if err := db.Migrate(ctx, migrations.Files); err != nil {
				return err
			}
			display := cfg.Workspace.DisplayName
			if display == "" {
				display = id
			}
			repo := cfg.Workspace.KnowledgeRepo
			if repo == "" {
				repo = "knowledge-" + id
			}
			if err := db.CreateWorkspace(ctx, postgres.Workspace{
				ID:            id,
				DisplayName:   display,
				KnowledgeRepo: repo,
				KnowledgeRoot: cfg.Workspace.KnowledgeRoot,
				RetentionDays: cfg.Workspace.RetentionDays,
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "initialized workspace %s\n", id)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file")
	cmd.Flags().StringVar(&workspaceID, "workspace", "", "workspace id")
	return cmd
}
