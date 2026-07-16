package cmd

import "github.com/spf13/cobra"

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the current state of managed database clusters",
	Long: `Reports the current, factual state of the managed clusters — the "what is?"
question: engine, namespace, whether HASteward manages it, last triage, last backup.

For interpretation — what is WRONG and what to do next — use 'triage', which runs the
diagnosis catalog and recommends a remedy.`,
	RunE: runStatus,
}

func init() {
	RootCmd.AddCommand(statusCmd)
}
