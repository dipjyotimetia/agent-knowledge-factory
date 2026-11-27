package main

import (
	"errors"
	"fmt"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/auth"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/traces"
	"github.com/spf13/cobra"
)

func newTraces() *cobra.Command {
	cmd := &cobra.Command{Use: "traces", Short: "Import a sanitized local trace"}
	var configPath, path, runID string
	importCmd := &cobra.Command{
		Use: "import", Args: cobra.NoArgs,
		Short: "Import a regular file for a configured workspace and run",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, workspace, err := loadOps(configPath, "")
			if err != nil {
				return err
			}
			if err := domain.ValidateWorkspaceID(workspace); err != nil {
				return err
			}
			owner := cfg.Traces.RunOwners[runID]
			if owner == "" || cfg.Traces.OperatorActorID == "" || cfg.DatabaseURL == "" {
				return errors.New("trace import requires a configured operator, run owner and application database")
			}
			raw, err := traces.ReadLocal(path)
			if err != nil {
				return err
			}
			db, err := postgres.Open(cmd.Context(), cfg.DatabaseURL)
			if err != nil {
				return err
			}
			defer db.Close()
			operator := auth.Principal{WorkspaceID: workspace, ActorID: cfg.Traces.OperatorActorID, Role: auth.RoleCurator}
			ref, err := (traces.Store{DB: db}).Import(cmd.Context(), operator, runID, owner, raw)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), ref)
			return err
		},
	}
	importCmd.Flags().StringVar(&configPath, "config", "", "trusted workspace config file")
	importCmd.Flags().StringVar(&path, "file", "", "local trace JSON file")
	importCmd.Flags().StringVar(&runID, "run-id", "", "configured run id")
	_ = importCmd.MarkFlagRequired("file")
	_ = importCmd.MarkFlagRequired("run-id")
	cmd.AddCommand(importCmd)
	return cmd
}
