package main

import (
	"fmt"
	"os"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/cmd"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"

	// Register engines via init()
	_ "gitlab.prplanit.com/precisionplanit/hasteward/src/engine/cnpg"
	_ "gitlab.prplanit.com/precisionplanit/hasteward/src/engine/galera"
)

func main() {
	common.InitLogging(false)
	if err := cmd.RootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
