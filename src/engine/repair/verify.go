package repair

import (
	"context"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
)

// How long VerifyRecovery waits for replication to establish before calling a heal
// incomplete. These bound a TRANSIENT window, not the heal itself — the heal's own
// waits have already completed by the time this gate runs.
//
// CNPG: a walreceiver connects within seconds of unfence, so the window exists for
// something else entirely — the primary restarting underneath the check. That is not
// hypothetical: a repair of grafana-postgres reseeded its replica correctly and then
// reported FAILED because the primary happened to be shutting down when the query
// landed, 2 seconds wide.
//
// Galera: a joining node may still be running an SST, which is bounded by the data
// size rather than by a protocol handshake, so it gets longer.
const (
	verifyCNPGTimeout   = 2 * time.Minute
	verifyGaleraTimeout = 5 * time.Minute
	verifyInterval      = 5 * time.Second
)

// awaitRecovery polls probe until it reports nothing pending, or the deadline passes.
//
// It exists because the engine gates were single-shot, which conflated two different
// things: "this instance is not replicating" and "this instance is not replicating
// YET". The first is the degraded cluster the gate exists to catch; the second is a
// handshake in progress, and failing on it turns a correct repair into a reported
// failure — which is worse than a slow one, because an operator who cannot trust a
// success is an operator who starts intervening by hand.
//
// It does NOT weaken the gate. The deadline is bounded and the result still fails
// closed: what changes is that a blip no longer counts as a verdict.
//
// Returns the last pending set and the last probe error. A nil pending set with a
// non-nil error means every attempt failed to read the cluster at all — a different
// diagnosis from "read it fine, these are not caught up", so callers distinguish them.
func awaitRecovery(ctx context.Context, timeout, interval time.Duration,
	probe func(context.Context) ([]string, error)) ([]string, error) {

	deadline := time.Now().Add(timeout)
	var (
		pending []string
		lastErr error
	)
	for attempt := 1; ; attempt++ {
		got, err := probe(ctx)
		switch {
		case err != nil:
			lastErr = err
			common.InfoLog("verify attempt %d: cannot read replication state yet (%v)", attempt, err)
		case len(got) == 0:
			return nil, nil
		default:
			pending = got
			lastErr = nil
			common.InfoLog("verify attempt %d: still waiting on %v", attempt, got)
		}
		if !time.Now().Add(interval).Before(deadline) {
			return pending, lastErr
		}
		// An interrupted run must stop waiting, and report what it last saw rather than
		// a cancellation: the caller's question is whether the cluster recovered.
		if err := ctx.Err(); err != nil {
			if lastErr == nil {
				lastErr = err
			}
			return pending, lastErr
		}
		common.Sleep(interval)
	}
}
