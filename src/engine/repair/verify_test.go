package repair

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
)

// noSleep removes the wait so the poll's control flow is tested, not the clock.
func noSleep(t *testing.T) {
	t.Helper()
	restore := common.SetSleepForTest(func(time.Duration) {})
	t.Cleanup(restore)
}

// The gate's job is to catch a cluster that is Ready but not replicating. The poll's job
// is to stop confusing that with one that is not replicating YET.
func TestAwaitRecovery(t *testing.T) {
	ctx := context.Background()

	t.Run("already recovered: one probe, no wait", func(t *testing.T) {
		noSleep(t)
		n := 0
		pending, err := awaitRecovery(ctx, time.Minute, time.Second, func(context.Context) ([]string, error) {
			n++
			return nil, nil
		})
		if pending != nil || err != nil {
			t.Fatalf("pending=%v err=%v", pending, err)
		}
		if n != 1 {
			t.Fatalf("a recovered cluster must not be polled twice, got %d probes", n)
		}
	})

	// The grafana-postgres case: the heal was correct and the primary was restarting.
	t.Run("transient read failures are waited through", func(t *testing.T) {
		noSleep(t)
		n := 0
		pending, err := awaitRecovery(ctx, time.Minute, time.Second, func(context.Context) ([]string, error) {
			n++
			if n < 3 {
				return nil, fmt.Errorf("FATAL: the database system is shutting down")
			}
			return nil, nil
		})
		if pending != nil || err != nil {
			t.Fatalf("a blip must not be a verdict: pending=%v err=%v", pending, err)
		}
		if n != 3 {
			t.Fatalf("want 3 probes, got %d", n)
		}
	})

	t.Run("a standby that connects late is waited through", func(t *testing.T) {
		noSleep(t)
		n := 0
		pending, err := awaitRecovery(ctx, time.Minute, time.Second, func(context.Context) ([]string, error) {
			n++
			if n < 2 {
				return []string{"c-2"}, nil
			}
			return nil, nil
		})
		if pending != nil || err != nil {
			t.Fatalf("pending=%v err=%v", pending, err)
		}
	})

	// Bounded: the gate still fails closed, it just no longer does so instantly.
	t.Run("never recovers: the pending set is reported", func(t *testing.T) {
		noSleep(t)
		pending, err := awaitRecovery(ctx, 10*time.Millisecond, time.Second, func(context.Context) ([]string, error) {
			return []string{"c-2"}, nil
		})
		if len(pending) != 1 || pending[0] != "c-2" {
			t.Fatalf("the degraded instance must still be named: %v", pending)
		}
		if err != nil {
			t.Fatalf("a clean read with a stranded instance is not a read error: %v", err)
		}
	})

	// The two diagnoses must stay distinguishable: "I could not look" is not the same
	// claim as "I looked and these are not caught up".
	t.Run("never readable: nil pending with the last error", func(t *testing.T) {
		noSleep(t)
		pending, err := awaitRecovery(ctx, 10*time.Millisecond, time.Second, func(context.Context) ([]string, error) {
			return nil, fmt.Errorf("connection refused")
		})
		if pending != nil {
			t.Fatalf("nothing was inspected, so nothing may be reported as stranded: %v", pending)
		}
		if err == nil {
			t.Fatal("the read failure must survive to the caller")
		}
	})

	// A success after a failure must clear the error, or a recovered cluster would be
	// reported as unverifiable.
	t.Run("a later success clears an earlier error", func(t *testing.T) {
		noSleep(t)
		n := 0
		// Interval under the timeout so the loop actually reaches a second probe; with
		// a 1s interval it would return after the first and never exercise the clear.
		pending, err := awaitRecovery(ctx, 5*time.Millisecond, time.Millisecond, func(context.Context) ([]string, error) {
			n++
			if n == 1 {
				return nil, fmt.Errorf("transient")
			}
			return []string{"c-2"}, nil
		})
		if err != nil {
			t.Fatalf("the stale error must be cleared by a successful read: %v", err)
		}
		if len(pending) != 1 {
			t.Fatalf("pending=%v", pending)
		}
	})
}
