package main

import (
	"context"
	"fmt"
	"os"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/evaluations"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
	"github.com/spf13/cobra"
)

func startInvestigation(workspace, gapID string) (bool, error) {
	cfg := workflows.TemporalFromLookup(os.Getenv)
	if cfg.Address == "" {
		return false, nil
	}
	starter, client, err := workflows.DialStarter(cfg, os.Getenv("TEMPORAL_TASK_QUEUE"), workspace)
	if err != nil {
		return false, err
	}
	defer client.Close()
	if err := starter.StartInvestigation(context.Background(), gapID); err != nil {
		return false, err
	}
	return true, nil
}

func newCurate() *cobra.Command {
	var configPath, workspace, gapID string
	cmd := &cobra.Command{
		Use:   "curate",
		Short: "Start curation for one investigation",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, id, err := loadOps(configPath, workspace)
			if err != nil {
				return err
			}
			if gapID == "" {
				return fmt.Errorf("gap id is required")
			}
			started, err := startInvestigation(id, gapID)
			if err != nil {
				return err
			}
			if !started {
				return fmt.Errorf("TEMPORAL_ADDRESS is unset; curation was not started")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "started %s\n", workflows.WorkflowID(id, gapID))
			_ = cfg
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	cmd.Flags().StringVar(&gapID, "gap", "", "canonical gap id")
	return cmd
}

func newEval() *cobra.Command {
	var configPath, workspace, cases string
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Score the golden case file against the published index",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, id, err := loadOps(configPath, workspace)
			if err != nil {
				return err
			}
			if cases == "" {
				cases = cfg.Curation.CasesPath
			}
			loaded, err := evaluations.Load(cases)
			if err != nil {
				return err
			}
			svc, _, err := openIndex(configPath, workspace)
			if err != nil {
				return err
			}
			defer svc.DB.Close()
			report, err := evaluations.RunRetrieval(context.Background(), loaded, func(ctx context.Context, query string) ([]evaluations.Hit, error) {
				hits, err := svc.Search(ctx, id, query, "", 3)
				if err != nil {
					return nil, err
				}
				var out []evaluations.Hit
				for _, hit := range hits {
					item, err := svc.Get(ctx, id, hit.URI, "")
					if err != nil {
						return nil, err
					}
					out = append(out, evaluations.Hit{URI: item.URI, Body: item.Body})
				}
				return out, nil
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "passed=%d failed=%d critical_regressions=%d\n", report.Passed, report.Failed, report.CriticalRegressions)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	cmd.Flags().StringVar(&cases, "cases", "", "case file")
	return cmd
}

func loadOps(configPath, workspace string) (config.Config, string, error) {
	if configPath == "" {
		configPath = os.Getenv("AKF_CONFIG")
	}
	cfg, err := config.Load(configPath, os.Getenv)
	if err != nil {
		return config.Config{}, "", err
	}
	id := workspace
	if id == "" {
		id = cfg.Workspace.ID
	}
	return cfg, id, nil
}
