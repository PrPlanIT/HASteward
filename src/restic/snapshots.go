package restic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Snapshot represents a restic snapshot.
type Snapshot struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Tags     []string  `json:"tags"`
	Paths    []string  `json:"paths"`
}

// TagMap returns snapshot tags as a key=value map.
func (s *Snapshot) TagMap() map[string]string {
	m := make(map[string]string)
	for _, t := range s.Tags {
		if idx := strings.Index(t, "="); idx > 0 {
			m[t[:idx]] = t[idx+1:]
		}
	}
	return m
}

// Snapshots returns snapshots matching the given tag filters (AND semantics).
func (c *Client) Snapshots(ctx context.Context, tags map[string]string) ([]Snapshot, error) {
	args := []string{"snapshots", "--json"}
	args = append(args, BuildTagFilter(tags)...)

	out, err := c.Run(ctx, args...)
	if err != nil {
		return nil, err
	}

	var snapshots []Snapshot
	if err := json.Unmarshal(out, &snapshots); err != nil {
		return nil, fmt.Errorf("failed to parse snapshots: %w", err)
	}
	return snapshots, nil
}
