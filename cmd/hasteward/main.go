package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/PrPlanIT/HASteward/src/cmd"
	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/output"
)

func main() {
	common.InitLogging(false)
	// Trap SIGINT/SIGTERM into context cancellation so an interrupted operation unwinds
	// through its normal return path — running deferred cleanup (e.g. resuming a suspended
	// operator CR) instead of being hard-killed with cleanup skipped, which strands the
	// operator suspended (#29).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cmd.RootCmd.ExecuteContext(ctx); err != nil {
		output.Stderr("Error: %v", err)
		os.Exit(1)
	}
}
