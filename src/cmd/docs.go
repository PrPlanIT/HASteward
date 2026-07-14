package cmd

import (
	"github.com/PrPlanIT/HASteward/internal/docsgen"
	"github.com/PrPlanIT/HASteward/src/output"

	"github.com/spf13/cobra"
)

var docsOutputDir string

var docsCmd = &cobra.Command{
	Use:   "docs",
	Short: "Documentation generation commands",
}

var docsGenerateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate reference documentation fragments from code",
	Long: `Generate reference documentation as markdown fragments — one per registered
generator (cli-reference, env-reference, ...) — into --output-dir. The fragments are
assembled into the docs site's reference pages by the pipeline's narrate step.

Run against the freshly-built binary so the output reflects the CLI and environment
bindings actually being shipped, never a stale copy.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		written, err := docsgen.GenerateAll(RootCmd, docsOutputDir)
		if err != nil {
			return err
		}
		for _, p := range written {
			output.Stderr("Generated: %s", p)
		}
		return nil
	},
}

func init() {
	docsGenerateCmd.Flags().StringVar(&docsOutputDir, "output-dir", "docs/assets/modules", "output directory for generated fragments")
	docsCmd.AddCommand(docsGenerateCmd)
	RootCmd.AddCommand(docsCmd)
}
