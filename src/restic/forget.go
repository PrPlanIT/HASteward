package restic

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// RetentionPolicy defines how many snapshots to keep.
type RetentionPolicy struct {
	KeepLast    int
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int
}

// ForgetResult holds the result of a forget operation for a tag group.
type ForgetResult struct {
	Tags   []string   `json:"tags"`
	Keep   []Snapshot `json:"keep"`
	Remove []Snapshot `json:"remove"`
}

// Forget removes snapshots according to the retention policy.
// If prune is true, unreferenced data is removed from the repository.
func (c *Client) Forget(ctx context.Context, tags map[string]string, policy RetentionPolicy, prune bool) ([]ForgetResult, error) {
	args := []string{"forget", "--json"}

	if policy.KeepLast > 0 {
		args = append(args, "--keep-last", strconv.Itoa(policy.KeepLast))
	}
	if policy.KeepDaily > 0 {
		args = append(args, "--keep-daily", strconv.Itoa(policy.KeepDaily))
	}
	if policy.KeepWeekly > 0 {
		args = append(args, "--keep-weekly", strconv.Itoa(policy.KeepWeekly))
	}
	if policy.KeepMonthly > 0 {
		args = append(args, "--keep-monthly", strconv.Itoa(policy.KeepMonthly))
	}

	args = append(args, BuildTagFilter(tags)...)

	if prune {
		args = append(args, "--prune")
	}

	out, err := c.Run(ctx, args...)
	if err != nil {
		return nil, err
	}

	var results []ForgetResult
	if err := json.Unmarshal(out, &results); err != nil {
		return nil, fmt.Errorf("failed to parse forget result: %w", err)
	}
	return results, nil
}

// ForgetSnapshot removes a single snapshot by ID.
func (c *Client) ForgetSnapshot(ctx context.Context, snapshotID string) error {
	_, err := c.Run(ctx, "forget", snapshotID)
	return err
}

// Prune removes unreferenced data from the repository.
func (c *Client) Prune(ctx context.Context) error {
	_, err := c.Run(ctx, "prune")
	return err
}

// JobGroup represents a set of diverged snapshots from the same repair job.
type JobGroup struct {
	JobID     string
	Time      time.Time
	Snapshots []Snapshot
}

// GroupByJob groups snapshots by their "job" tag value, sorted newest first.
// Snapshots without a job tag are each treated as their own group.
func GroupByJob(snapshots []Snapshot) []JobGroup {
	groups := make(map[string]*JobGroup)
	for _, s := range snapshots {
		tags := s.TagMap()
		jobID := tags["job"]
		if jobID == "" {
			// Ungrouped: treat as its own group
			jobID = "ungrouped-" + s.ShortID
		}
		if g, ok := groups[jobID]; ok {
			g.Snapshots = append(g.Snapshots, s)
		} else {
			groups[jobID] = &JobGroup{
				JobID:     jobID,
				Time:      s.Time,
				Snapshots: []Snapshot{s},
			}
		}
	}
	result := make([]JobGroup, 0, len(groups))
	for _, g := range groups {
		result = append(result, *g)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Time.After(result[j].Time)
	})
	return result
}

// ApplyGroupRetention applies retention policies to job groups.
// Policies work on groups as units: keep-last keeps N most recent jobs,
// keep-daily/weekly/monthly keep one job per time window.
// If all policy values are zero, all groups are kept.
func ApplyGroupRetention(groups []JobGroup, policy RetentionPolicy) (keep, remove []JobGroup) {
	if len(groups) == 0 {
		return nil, nil
	}
	// All zeros = keep everything (matches restic behavior)
	if policy.KeepLast == 0 && policy.KeepDaily == 0 &&
		policy.KeepWeekly == 0 && policy.KeepMonthly == 0 {
		return groups, nil
	}

	keepSet := make(map[string]bool)

	if policy.KeepLast > 0 {
		for i, g := range groups {
			if i >= policy.KeepLast {
				break
			}
			keepSet[g.JobID] = true
		}
	}

	if policy.KeepDaily > 0 {
		seen := 0
		lastDay := ""
		for _, g := range groups {
			day := g.Time.UTC().Format("2006-01-02")
			if day != lastDay {
				lastDay = day
				seen++
				if seen > policy.KeepDaily {
					break
				}
				keepSet[g.JobID] = true
			}
		}
	}

	if policy.KeepWeekly > 0 {
		seen := 0
		lastWeek := ""
		for _, g := range groups {
			y, w := g.Time.UTC().ISOWeek()
			week := fmt.Sprintf("%d-%02d", y, w)
			if week != lastWeek {
				lastWeek = week
				seen++
				if seen > policy.KeepWeekly {
					break
				}
				keepSet[g.JobID] = true
			}
		}
	}

	if policy.KeepMonthly > 0 {
		seen := 0
		lastMonth := ""
		for _, g := range groups {
			month := g.Time.UTC().Format("2006-01")
			if month != lastMonth {
				lastMonth = month
				seen++
				if seen > policy.KeepMonthly {
					break
				}
				keepSet[g.JobID] = true
			}
		}
	}

	for _, g := range groups {
		if keepSet[g.JobID] {
			keep = append(keep, g)
		} else {
			remove = append(remove, g)
		}
	}
	return
}

// ForgetGrouped applies group-aware retention for diverged snapshots.
// Snapshots sharing a "job" tag are treated as a single unit for retention.
// Returns the number of snapshots kept and removed.
func (c *Client) ForgetGrouped(ctx context.Context, tags map[string]string, policy RetentionPolicy, prune bool) (keepCount, removeCount int, err error) {
	snapshots, err := c.Snapshots(ctx, tags)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to list snapshots: %w", err)
	}

	groups := GroupByJob(snapshots)
	keep, remove := ApplyGroupRetention(groups, policy)

	for _, g := range keep {
		keepCount += len(g.Snapshots)
	}

	for _, g := range remove {
		for _, s := range g.Snapshots {
			if err := c.ForgetSnapshot(ctx, s.ID); err != nil {
				return keepCount, removeCount, fmt.Errorf("failed to forget snapshot %s: %w", s.ShortID, err)
			}
			removeCount++
		}
	}

	if prune && removeCount > 0 {
		if err := c.Prune(ctx); err != nil {
			return keepCount, removeCount, fmt.Errorf("prune failed: %w", err)
		}
	}

	return keepCount, removeCount, nil
}
