package cmd

import (
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"
	"github.com/PrPlanIT/HASteward/src/output/printer"
	"github.com/PrPlanIT/HASteward/src/version"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the hasteward version and build info",
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := InitPrinter("version")
		if err != nil {
			return err
		}
		info := &model.VersionInfo{
			Version:   version.Version,
			Commit:    version.Commit,
			BuildDate: version.BuildDate,
		}
		if p.IsHuman() {
			output.Printf("hasteward %s\n", info.Version)
			output.Field("Commit", info.Commit)
			output.Field("Built", info.BuildDate)
			return nil
		}
		printer.PrintResult(p, info, nil, nil)
		return nil
	},
}

func init() {
	RootCmd.AddCommand(versionCmd)
}
