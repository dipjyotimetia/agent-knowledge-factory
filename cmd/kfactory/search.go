package main

import (
	"context"
	"fmt"
	"os"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publication"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/runtime"
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
	var bootstrap bool
	cmd := &cobra.Command{
		Use: "index", Short: "Queue validation of the authoritative Git tree",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if root != "" {
				return fmt.Errorf("local roots are unsupported by the release queue")
			}
			cfg, id, err := loadOps(configPath, workspace)
			if err != nil {
				return err
			}
			if id != cfg.Workspace.ID {
				return publication.ErrIdentity
			}
			db, err := postgres.Open(cmd.Context(), cfg.DatabaseURL)
			if err != nil {
				return err
			}
			defer db.Close()
			svc := index.New(db)
			svc.Hybrid = os.Getenv("SEARCH_MODE") == "hybrid"
			releases, err := runtime.NewPublication(cfg, db, svc, os.Getenv)
			if err != nil {
				return err
			}
			trigger := "operator"
			if bootstrap {
				trigger = "bootstrap"
			}
			op, err := releases.RequestRelease(cmd.Context(), publication.Request{Trigger: trigger, ObservedSHA: sha})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pending %s\n", op)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	cmd.Flags().StringVar(&sha, "sha", "", "observed Git SHA")
	cmd.Flags().StringVar(&root, "root", "", "legacy local directory option")
	cmd.Flags().BoolVar(&bootstrap, "bootstrap", false, "queue held bootstrap")
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
