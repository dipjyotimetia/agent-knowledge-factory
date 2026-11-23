package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/spf13/cobra"
)

func newGaps() *cobra.Command {
	cmd := &cobra.Command{Use: "gaps", Short: "List and triage gap reports"}
	cmd.AddCommand(newGapsList(), newGapsShow(), newGapsTriage(), newGapsReport())
	return cmd
}

func newGapsList() *cobra.Command {
	var configPath, workspace string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List gap reports for a workspace",
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, id, err := openGaps(configPath, workspace)
			if err != nil {
				return err
			}
			defer svc.DB.Close()
			list, err := svc.List(context.Background(), id)
			if err != nil {
				return err
			}
			for _, gap := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", gap.ID, gap.State, gap.Report.Summary)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	return cmd
}

func newGapsShow() *cobra.Command {
	var configPath, workspace string
	cmd := &cobra.Command{
		Use:   "show [id]",
		Short: "Show one gap and the submissions linked to its investigation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, id, err := openGaps(configPath, workspace)
			if err != nil {
				return err
			}
			defer svc.DB.Close()
			canonical, related, err := svc.Linked(context.Background(), id, args[0])
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(map[string]any{"canonical": canonical, "linked": related})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	return cmd
}

func newGapsTriage() *cobra.Command {
	var configPath, workspace, reason string
	cmd := &cobra.Command{
		Use:   "triage [id]",
		Short: "Move a new gap to triaged",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, id, err := openGaps(configPath, workspace)
			if err != nil {
				return err
			}
			defer svc.DB.Close()
			if reason == "" {
				reason = "triaged from the CLI"
			}
			gap, err := svc.Transition(context.Background(), id, args[0], domain.StateTriaged, "cli", reason)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", gap.ID, gap.State)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	cmd.Flags().StringVar(&reason, "reason", "", "why the gap is triaged")
	return cmd
}

func newGapsReport() *cobra.Command {
	var configPath, workspace, file, agent string
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Submit a gap report JSON file",
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := os.ReadFile(file) // #nosec G304 -- path is the operator --file flag, not request input
			if err != nil {
				return err
			}
			var report domain.GapReport
			if err := json.Unmarshal(raw, &report); err != nil {
				return err
			}
			svc, id, err := openGaps(configPath, workspace)
			if err != nil {
				return err
			}
			defer svc.DB.Close()
			if report.WorkspaceID == "" {
				report.WorkspaceID = id
			}
			if agent == "" {
				agent = "cli"
			}
			gap, err := svc.Report(context.Background(), report, agent)
			if err != nil {
				return err
			}
			target := gap.ID
			if gap.CanonicalID != "" {
				target = gap.CanonicalID
			}
			if err := startInvestigation(id, target); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), gap.ID, gap.State)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	cmd.Flags().StringVar(&file, "file", "", "gap report JSON")
	cmd.Flags().StringVar(&agent, "agent", "cli", "agent id stored with the report")
	return cmd
}

func openGaps(configPath, workspace string) (*gaps.Service, string, error) {
	svc, id, err := openIndex(configPath, workspace)
	if err != nil {
		return nil, "", err
	}
	return gaps.NewService(svc.DB), id, nil
}
