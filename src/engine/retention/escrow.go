package retention

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/escrow"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// escrowGroup is every PVC escrowed by one run. Retention works on groups, never
// on individual snapshots: a run that escrowed three PVCs is one rollback point,
// and releasing part of it leaves a rollback that cannot actually be performed.
type escrowGroup struct {
	RunID      string
	Kind       string
	CapturedAt time.Time
	Snapshots  []string
}

// pruneEscrow releases escrow VolumeSnapshots that are past the retention policy.
//
// Escrow retention is deliberately not part of "all". An escrow is the rollback
// for a mutation that already happened, and on this estate the matched snapshot
// class carries deletionPolicy: Delete — releasing the object destroys the
// underlying storage snapshot. So it is opt-in, it is bounded below by a minimum
// age no policy can override, and it only ever touches objects this tool labelled.
func pruneEscrow(ctx context.Context, cfg *common.Config, opts PruneOptions) (kept, removed int, err error) {
	c := k8s.GetClients()
	ns := cfg.Namespace

	list, err := c.Dynamic.Resource(k8s.VolumeSnapshotGVR).Namespace(ns).List(ctx, metav1.ListOptions{
		LabelSelector: escrow.ClusterSelector(cfg.ClusterName),
	})
	if err != nil {
		return 0, 0, fmt.Errorf("listing escrow VolumeSnapshots: %w", err)
	}

	groups := groupEscrows(list.Items)
	if len(groups) == 0 {
		output.Bullet(0, "No escrow snapshots for %s/%s", ns, cfg.ClusterName)
		reportUnmanaged(ctx, ns, cfg.ClusterName)
		return 0, 0, nil
	}

	// Newest first, so "keep the last N" is the first N.
	sort.Slice(groups, func(i, j int) bool { return groups[i].CapturedAt.After(groups[j].CapturedAt) })

	held, release := selectForRelease(groups, opts.KeepLast, opts.EscrowMinAge, time.Now())
	for _, g := range held.WithinMinAge {
		output.Bullet(1, "Holding escrow %s (%s, age %s) — inside the %s minimum",
			escrowShort(g.RunID), g.Kind, age(g.CapturedAt), opts.EscrowMinAge)
	}
	kept = held.Count

	for _, g := range release {
		for _, name := range g.Snapshots {
			if cfg.DryRun {
				output.Plan("DRY RUN: would RELEASE escrow VolumeSnapshot %s/%s (run %s, %s, age %s). "+
					"The snapshot class deletes the backing storage snapshot with the object.",
					ns, name, escrowShort(g.RunID), g.Kind, age(g.CapturedAt))
				removed++
				continue
			}
			err := c.Dynamic.Resource(k8s.VolumeSnapshotGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				common.WarnLog("Failed to release escrow VolumeSnapshot %s: %v", name, err)
				continue
			}
			output.Bullet(1, "Released escrow %s (run %s, %s, age %s)", name, escrowShort(g.RunID), g.Kind, age(g.CapturedAt))
			removed++
		}
	}

	reportUnmanaged(ctx, ns, cfg.ClusterName)
	return kept, removed, nil
}

// groupEscrows folds the snapshot list into one entry per run, taking the group's
// capture time from the stamped annotation and falling back to the object's own
// creation timestamp for escrows taken before the annotation existed.
func groupEscrows(items []unstructured.Unstructured) []escrowGroup {
	byRun := map[string]*escrowGroup{}
	for i := range items {
		obj := &items[i]
		labels := obj.GetLabels()
		runID := labels[escrow.LabelRunID]
		if runID == "" {
			// No run id: treat the snapshot as its own group so it still ages out
			// rather than becoming immortal for want of a label.
			runID = "unruled-" + obj.GetName()
		}
		g, ok := byRun[runID]
		if !ok {
			g = &escrowGroup{RunID: runID, Kind: labels[escrow.LabelKind], CapturedAt: capturedAt(obj)}
			if g.Kind == "" {
				g.Kind = "unknown"
			}
			byRun[runID] = g
		}
		g.Snapshots = append(g.Snapshots, obj.GetName())
		// A group is only as old as its youngest member: the rollback point is not
		// spent until every PVC in it is.
		if t := capturedAt(obj); t.After(g.CapturedAt) {
			g.CapturedAt = t
		}
	}
	out := make([]escrowGroup, 0, len(byRun))
	for _, g := range byRun {
		sort.Strings(g.Snapshots)
		out = append(out, *g)
	}
	return out
}

// heldEscrows is what retention decided to keep, and why.
type heldEscrows struct {
	Count        int           // snapshots kept, across all reasons
	WithinMinAge []escrowGroup // kept only because they are younger than the floor
}

// selectForRelease decides which escrow groups may be released. It is pure so the
// decision that destroys storage can be tested without a cluster.
//
// Two rules, in order: the most recent KeepLast runs are always kept, and nothing
// younger than minAge is ever released regardless of count. The age floor is not a
// tiebreaker — it overrides the count policy, because an escrow younger than the
// floor is still inside the window where an operator may discover the repair it
// protects was the wrong call.
func selectForRelease(groups []escrowGroup, keepLast int, minAge time.Duration, now time.Time) (heldEscrows, []escrowGroup) {
	cutoff := now.Add(-minAge)
	var held heldEscrows
	var release []escrowGroup
	for i, g := range groups {
		switch {
		case i < keepLast:
			held.Count += len(g.Snapshots)
		case g.CapturedAt.After(cutoff):
			held.Count += len(g.Snapshots)
			held.WithinMinAge = append(held.WithinMinAge, g)
		default:
			release = append(release, g)
		}
	}
	return held, release
}

func capturedAt(obj *unstructured.Unstructured) time.Time {
	if v := obj.GetAnnotations()[escrow.AnnCapturedAt]; v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
	}
	return obj.GetCreationTimestamp().Time
}

// reportUnmanaged names snapshots that look like escrows but carry none of this
// tool's labels. They are never released: something else made them, and on a
// delete-policy snapshot class a wrong guess destroys storage. Reporting them is
// the whole remedy — an operator can see them and decide.
func reportUnmanaged(ctx context.Context, ns, cluster string) {
	c := k8s.GetClients()
	list, err := c.Dynamic.Resource(k8s.VolumeSnapshotGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	var orphans []string
	for i := range list.Items {
		obj := &list.Items[i]
		name := obj.GetName()
		if !strings.Contains(name, "escrow") && !strings.Contains(name, "preprune") {
			continue
		}
		if obj.GetLabels()[escrow.LabelEscrow] == "true" {
			continue
		}
		if cluster != "" && !strings.Contains(name, cluster) {
			continue
		}
		orphans = append(orphans, name)
	}
	if len(orphans) == 0 {
		return
	}
	sort.Strings(orphans)
	common.WarnLog("%d escrow-shaped VolumeSnapshot(s) in %s carry no hasteward escrow labels and were NOT touched: %s. "+
		"They predate labelled escrow or were taken by hand; release them deliberately once you know they are spent.",
		len(orphans), ns, strings.Join(orphans, ", "))
}

func escrowShort(runID string) string {
	if len(runID) > 8 {
		return runID[:8]
	}
	return runID
}

func age(t time.Time) string {
	d := time.Since(t).Round(time.Hour)
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
