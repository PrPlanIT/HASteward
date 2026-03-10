package cmd

import (
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output"

	"github.com/spf13/cobra"
)

var triageCmd = &cobra.Command{
	Use:   "triage",
	Short: "Read-only diagnostics for a database cluster",
	RunE: func(cmd *cobra.Command, args []string) error {
		eng, err := PreRun(cmd, "triage")
		if err != nil {
			return err
		}

		if _, err := eng.Triage(cmd.Context()); err != nil {
			return err
		}

		output.Complete("Triage complete")
		return nil
	},
}
