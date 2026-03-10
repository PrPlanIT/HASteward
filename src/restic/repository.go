package restic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Init initializes a new restic repository. Returns nil if already initialized.
func (c *Client) Init(ctx context.Context) error {
	_, err := c.Run(ctx, "init")
	if err != nil {
		if strings.Contains(err.Error(), "already initialized") ||
			strings.Contains(err.Error(), "already exists") {
			return nil
		}
		return fmt.Errorf("restic init: %w", err)
	}
	return nil
}

// Check verifies repository integrity.
func (c *Client) Check(ctx context.Context) error {
	_, err := c.Run(ctx, "check")
	return err
}

// Unlock removes stale repository locks.
func (c *Client) Unlock(ctx context.Context) error {
	_, err := c.Run(ctx, "unlock")
	return err
}

// RepoStats holds repository statistics from restic stats.
type RepoStats struct {
	TotalSize      int64 `json:"total_size"`
	TotalFileCount int   `json:"total_file_count"`
}

// Stats returns repository statistics.
func (c *Client) Stats(ctx context.Context) (*RepoStats, error) {
	out, err := c.Run(ctx, "stats", "--json")
	if err != nil {
		return nil, err
	}

	var stats RepoStats
	if err := json.Unmarshal(out, &stats); err != nil {
		return nil, fmt.Errorf("failed to parse restic stats: %w", err)
	}
	return &stats, nil
}
