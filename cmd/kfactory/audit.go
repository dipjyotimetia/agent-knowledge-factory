package main

import (
	"context"
	"fmt"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/audit"
	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"
)

func newAudit() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Inspect the hash-chained audit log",
	}
	cmd.AddCommand(newAuditVerify())
	return cmd
}

func newAuditVerify() *cobra.Command {
	var configPath, workspace string
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Check that each audit hash commits to the previous row",
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, id, err := openIndex(configPath, workspace)
			if err != nil {
				return err
			}
			defer svc.DB.Close()
			err = svc.DB.WithWorkspace(context.Background(), id, func(ctx context.Context, tx pgx.Tx) error {
				return audit.Verify(ctx, tx, id)
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "audit chain ok")
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	return cmd
}

func newRollback() *cobra.Command {
	var configPath, workspace, sha string
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Point search at an already indexed revision",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if sha == "" {
				return fmt.Errorf("sha is required")
			}
			svc, id, err := openIndex(configPath, workspace)
			if err != nil {
				return err
			}
			defer svc.DB.Close()
			if err := svc.Rollback(context.Background(), id, sha, "kfactory"); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "rolled back to %s\n", sha)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	cmd.Flags().StringVar(&sha, "sha", "", "indexed revision to publish again")
	return cmd
}
