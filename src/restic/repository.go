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

// Exists reports whether the repository is already present and openable, WITHOUT
// creating one. Init creates on demand, which is right for a first `backup create`
// but wrong for an escrow — so callers that must not invent their own store ask this
// first. A missing repository and an unreachable backend demand opposite responses,
// so only the former returns (false, nil); anything else is surfaced as an error.
func (c *Client) Exists(ctx context.Context) (bool, error) {
	if _, err := c.Run(ctx, "cat", "config"); err != nil {
		e := err.Error()
		for _, missing := range []string{
			"unable to open config file",
			"repository does not exist",
			"no such file or directory",
			"Is there a repository at the following location?",
		} {
			if strings.Contains(e, missing) {
				return false, nil
			}
		}
		return false, err
	}
	return true, nil
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
