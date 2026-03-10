package restic

import (
	"context"
	"io"
)

// Dump extracts a file from a snapshot and writes its content to stdout.
// snapshotID can be a specific ID or "latest".
// path is the file path within the snapshot (e.g., "zeldas-lullaby/zitadel-postgres/pgdumpall.sql").
// tags are used to filter which "latest" snapshot to select (AND semantics).
func (c *Client) Dump(ctx context.Context, snapshotID, path string, stdout io.Writer, tags map[string]string) error {
	args := []string{"dump"}
	args = append(args, BuildTagFilter(tags)...)
	args = append(args, snapshotID, path)
	return c.RunWithStdout(ctx, stdout, args...)
}
