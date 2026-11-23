package main

import (
	"context"
	"fmt"
	"os"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/spf13/cobra"
)

func newSearch() *cobra.Command {
	var configPath, workspace, itemType string
	var limit int
	cmd := &cobra.Command{
		Use:   "search [query]",
		Short: "Search the published knowledge index",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, id, err := openIndex(configPath, workspace)
			if err != nil {
				return err
			}
			defer svc.DB.Close()
			hits, err := svc.Search(context.Background(), id, args[0], itemType, limit)
			if err != nil {
				return err
			}
			for _, hit := range hits {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", hit.URI, hit.Revision, hit.Title)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	cmd.Flags().StringVar(&itemType, "type", "", "concept or skill")
	cmd.Flags().IntVar(&limit, "limit", 10, "maximum results")
	return cmd
}

func newIndexCmd() *cobra.Command {
	var configPath, workspace, sha, root string
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Rebuild the published index from a knowledge directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, id, err := openIndex(configPath, workspace)
			if err != nil {
				return err
			}
			defer svc.DB.Close()
			if sha == "" {
				return fmt.Errorf("sha is required")
			}
			if root == "" {
				root = "examples/banking/fixture-knowledge"
			}
			if err := svc.Reindex(context.Background(), id, sha, root); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "published %s for %s\n", sha, id)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	cmd.Flags().StringVar(&sha, "sha", "", "git SHA to publish")
	cmd.Flags().StringVar(&root, "root", "", "knowledge directory")
	return cmd
}

func openIndex(configPath, workspace string) (*index.Service, string, error) {
	if configPath == "" {
		configPath = os.Getenv("AKF_CONFIG")
	}
	cfg, err := config.Load(configPath, os.Getenv)
	if err != nil {
		return nil, "", err
	}
	id := workspace
	if id == "" {
		id = cfg.Workspace.ID
	}
	if err := domain.ValidateWorkspaceID(id); err != nil {
		return nil, "", err
	}
	dsn := cfg.DatabaseURL
	if dsn == "" {
		dsn = cfg.InstallerDatabaseURL
	}
	if dsn == "" {
		return nil, "", fmt.Errorf("set AKF_DATABASE_URL")
	}
	db, err := postgres.Open(context.Background(), dsn)
	if err != nil {
		return nil, "", err
	}
	svc := index.New(db)
	svc.Hybrid = os.Getenv("SEARCH_MODE") == "hybrid"
	return svc, id, nil
}
