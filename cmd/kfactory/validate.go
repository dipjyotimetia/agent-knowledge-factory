package main

import (
	"fmt"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/knowledge/okf"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/knowledge/skills"
	"github.com/spf13/cobra"
)

func newValidate() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [path]",
		Short: "Validate an OKF v0.3 bundle and any Agent Skills under it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bundle, err := okf.Load(args[0], okf.Options{})
			if err != nil {
				return err
			}
			for _, d := range bundle.Diagnostics {
				fmt.Fprintln(cmd.OutOrStdout(), d.String())
			}
			loaded, err := skills.LoadTree(args[0])
			if err != nil {
				return err
			}
			ok := bundle.Conformant()
			for _, skill := range loaded {
				for _, d := range skill.Diagnostics {
					fmt.Fprintf(cmd.OutOrStdout(), "%s: %s: %s\n", skill.Name, d.Severity, d.Message)
				}
				if !skill.Valid() {
					ok = false
				}
			}
			if !ok {
				return fmt.Errorf("validation failed")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "valid: %d concepts, %d skills\n", len(bundle.Concepts), len(loaded))
			return nil
		},
	}
}
