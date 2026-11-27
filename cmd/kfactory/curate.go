package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/evaluations"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/investigation"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/runtime"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/storage/postgres"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
	"github.com/spf13/cobra"
)

func newCurate() *cobra.Command {
	var configPath, workspace, gapID, resumeID string
	var importLegacy bool
	cmd := &cobra.Command{
		Use:   "curate",
		Short: "Queue or resume an investigation attempt",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, id, err := loadOps(configPath, workspace)
			if err != nil {
				return err
			}
			if id != cfg.Workspace.ID {
				return investigation.ErrMismatch
			}
			if cfg.DatabaseURL == "" {
				return fmt.Errorf("AKF_DATABASE_URL is required to queue investigations")
			}
			if os.Getenv("AKF_PAUSE_INTAKE") == "1" && !importLegacy {
				return fmt.Errorf("investigation intake is paused")
			}
			ctx := cmd.Context()
			db, err := postgres.Open(ctx, cfg.DatabaseURL)
			if err != nil {
				return err
			}
			defer db.Close()
			store := runtime.NewInvestigationStore(cfg, db)
			if importLegacy {
				if os.Getenv("AKF_PAUSE_INTAKE") != "1" {
					return fmt.Errorf("pause intake before importing legacy mappings")
				}
				return importLegacyAttempts(cmd, cfg, db, store)
			}
			if resumeID != "" {
				temporalClient, err := workflows.Dial(workflows.TemporalFromLookup(os.Getenv))
				if err != nil {
					return err
				}
				defer temporalClient.Close()
				attempt, err := store.Resume(ctx, resumeID)
				if err != nil {
					return err
				}
				if err = temporalClient.SignalWorkflow(ctx, investigation.WorkflowID(attempt), "", workflows.SignalResume, true); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "resumed", attempt.AttemptID)
				return nil
			}
			if gapID == "" {
				return fmt.Errorf("gap id is required")
			}
			attempt, err := store.Enqueue(ctx, gapID)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "pending", attempt.AttemptID)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	cmd.Flags().StringVar(&gapID, "gap", "", "canonical gap id")
	cmd.Flags().StringVar(&resumeID, "resume", "", "parked attempt id")
	cmd.Flags().BoolVar(&importLegacy, "import-legacy", false, "verify and import legacy review mappings")
	cmd.MarkFlagsMutuallyExclusive("gap", "resume", "import-legacy")
	return cmd
}

func importLegacyAttempts(cmd *cobra.Command, cfg config.Config, db *postgres.DB, store investigation.Store) error {
	releases, err := runtime.NewPublication(cfg, db, index.New(db), os.Getenv)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	after := ""
	for {
		proposals, err := store.LegacyPage(ctx, after)
		if err != nil {
			return err
		}
		for _, p := range proposals {
			verify := releases.VerifyLegacyMapping
			if p.Draft {
				verify = releases.LegacyDraftHasNoPR
			}
			valid, err := verify(ctx, p.ProposalID, p.GapID)
			if err != nil {
				var forgeErr *publish.HTTPError
				if p.Draft || !errors.As(err, &forgeErr) || forgeErr.Status != 404 {
					return err
				}
			}
			if err = store.Import(ctx, p, valid); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s verified=%t\n", p.ProposalID, valid)
			after = p.ProposalID
		}
		if len(proposals) < 50 {
			return nil
		}
	}
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
