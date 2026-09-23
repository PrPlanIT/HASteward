package retention

import (
	"testing"
	"time"

	"github.com/PrPlanIT/HASteward/src/engine/escrow"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func at(now time.Time, daysAgo int) time.Time {
	return now.Add(-time.Duration(daysAgo) * 24 * time.Hour)
}

func grp(runID string, capturedAt time.Time, snaps ...string) escrowGroup {
	return escrowGroup{RunID: runID, Kind: escrow.KindSplitBrain, CapturedAt: capturedAt, Snapshots: snaps}
}

// The count policy keeps the newest runs and releases the rest once they are past
// the age floor.
func TestSelectForReleaseKeepsNewestRuns(t *testing.T) {
	now := time.Now()
	groups := []escrowGroup{
		grp("a", at(now, 1), "s1"),
		grp("b", at(now, 20), "s2"),
		grp("c", at(now, 30), "s3"),
	}
	held, release := selectForRelease(groups, 1, 7*24*time.Hour, now)
	if held.Count != 1 {
		t.Fatalf("kept %d snapshots, want 1", held.Count)
	}
	if len(release) != 2 {
		t.Fatalf("released %d groups, want 2", len(release))
	}
	if release[0].RunID != "b" || release[1].RunID != "c" {
		t.Fatalf("released %q,%q; want b,c", release[0].RunID, release[1].RunID)
	}
}

// The age floor overrides the count policy: a run past --keep-last is still held
// while it is younger than the minimum.
func TestSelectForReleaseMinAgeOverridesCount(t *testing.T) {
	now := time.Now()
	groups := []escrowGroup{
		grp("fresh1", at(now, 1), "s1"),
		grp("fresh2", at(now, 2), "s2"),
		grp("fresh3", at(now, 3), "s3"),
	}
	held, release := selectForRelease(groups, 1, 7*24*time.Hour, now)
	if len(release) != 0 {
		t.Fatalf("released %d groups; nothing inside the min age may be released", len(release))
	}
	if held.Count != 3 {
		t.Fatalf("kept %d, want 3", held.Count)
	}
	if len(held.WithinMinAge) != 2 {
		t.Fatalf("reported %d held-by-age, want 2", len(held.WithinMinAge))
	}
}

// A run that escrowed several PVCs is one rollback point: it is released whole or
// not at all, and its snapshots all count toward the kept total.
func TestSelectForReleaseTreatsRunAsOneUnit(t *testing.T) {
	now := time.Now()
	groups := []escrowGroup{
		grp("new", at(now, 1), "n1", "n2", "n3"),
		grp("old", at(now, 40), "o1", "o2", "o3"),
	}
	held, release := selectForRelease(groups, 1, 7*24*time.Hour, now)
	if held.Count != 3 {
		t.Fatalf("kept %d, want all 3 snapshots of the kept run", held.Count)
	}
	if len(release) != 1 || len(release[0].Snapshots) != 3 {
		t.Fatalf("expected one released run carrying all 3 of its snapshots, got %+v", release)
	}
}

// keep-last 0 is still bounded by the age floor, so a misconfigured policy cannot
// destroy a rollback point that is still inside its window.
func TestSelectForReleaseZeroKeepLastStillHonoursFloor(t *testing.T) {
	now := time.Now()
	groups := []escrowGroup{grp("a", at(now, 1), "s1"), grp("b", at(now, 40), "s2")}
	held, release := selectForRelease(groups, 0, 7*24*time.Hour, now)
	if len(release) != 1 || release[0].RunID != "b" {
		t.Fatalf("want only the aged run released, got %+v", release)
	}
	if held.Count != 1 {
		t.Fatalf("kept %d, want the fresh run held by the floor", held.Count)
	}
}

func snapObj(name, runID, kind, capturedAt string, creation time.Time) unstructured.Unstructured {
	m := map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata": map[string]interface{}{
			"name":              name,
			"creationTimestamp": creation.UTC().Format(time.RFC3339),
		},
	}
	obj := unstructured.Unstructured{Object: m}
	labels := map[string]string{escrow.LabelEscrow: "true"}
	if runID != "" {
		labels[escrow.LabelRunID] = runID
	}
	if kind != "" {
		labels[escrow.LabelKind] = kind
	}
	obj.SetLabels(labels)
	if capturedAt != "" {
		obj.SetAnnotations(map[string]string{escrow.AnnCapturedAt: capturedAt})
	}
	return obj
}

// Snapshots sharing a run id fold into one group, and the group is only as old as
// its youngest member — a rollback point is not spent until every PVC in it is.
func TestGroupEscrowsFoldsByRunAndTakesYoungest(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	older := now.Add(-48 * time.Hour)
	items := []unstructured.Unstructured{
		snapObj("s1", "run1", escrow.KindDeadlock, older.Format(time.RFC3339), older),
		snapObj("s2", "run1", escrow.KindDeadlock, now.Format(time.RFC3339), now),
	}
	groups := groupEscrows(items)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if len(groups[0].Snapshots) != 2 {
		t.Fatalf("group holds %d snapshots, want 2", len(groups[0].Snapshots))
	}
	if !groups[0].CapturedAt.Equal(now) {
		t.Fatalf("group captured-at %s, want the youngest member %s", groups[0].CapturedAt, now)
	}
}

// An escrow taken before the capture-time annotation existed still ages out, using
// the object's own creation timestamp.
func TestGroupEscrowsFallsBackToCreationTimestamp(t *testing.T) {
	created := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Second)
	groups := groupEscrows([]unstructured.Unstructured{snapObj("s1", "run1", "", "", created)})
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if !groups[0].CapturedAt.Equal(created) {
		t.Fatalf("captured-at %s, want creation timestamp %s", groups[0].CapturedAt, created)
	}
	if groups[0].Kind != "unknown" {
		t.Fatalf("kind %q, want unknown for an unlabelled capture", groups[0].Kind)
	}
}

// A snapshot with no run id becomes its own group rather than being merged with
// every other unlabelled one, so a missing label cannot make it immortal or drag
// unrelated escrows into one release decision.
func TestGroupEscrowsIsolatesMissingRunID(t *testing.T) {
	now := time.Now().UTC()
	groups := groupEscrows([]unstructured.Unstructured{
		snapObj("s1", "", "", "", now),
		snapObj("s2", "", "", "", now),
	})
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2 distinct groups for unlabelled snapshots", len(groups))
	}
}
