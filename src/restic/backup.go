package restic

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// BackupSummary holds the result of a restic backup --stdin --json operation.
type BackupSummary struct {
	SnapshotID    string  `json:"snapshot_id"`
	FilesNew      int     `json:"files_new"`
	FilesChanged  int     `json:"files_changed"`
	DataAdded     int64   `json:"data_added"`
	TotalSize     int64   `json:"total_bytes_processed"`
	TotalDuration float64 `json:"total_duration"`
}

// BackupStdin runs restic backup --stdin, reading data from the provided reader.
// stdinFilename sets the virtual file path in the snapshot.
// tags are key=value metadata added to the snapshot.
// jobTime sets the snapshot timestamp via --time (use the job start time for consistency).
func (c *Client) BackupStdin(ctx context.Context, stdin io.Reader, stdinFilename string, tags map[string]string, jobTime time.Time) (*BackupSummary, error) {
	args := []string{"backup", "--stdin", "--stdin-filename", stdinFilename, "--json",
		"--time", jobTime.UTC().Format("2006-01-02 15:04:05")}
	args = append(args, BuildTagArgs(tags)...)

	cmd := exec.CommandContext(ctx, c.binary(), args...)
	cmd.Env = append(os.Environ(), c.buildEnv()...)
	cmd.Stdin = stdin
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start restic backup: %w", err)
	}

	// Parse JSON output lines looking for the summary message
	var summary *BackupSummary
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var msg struct {
			MessageType string `json:"message_type"`
		}
		line := scanner.Bytes()
		if json.Unmarshal(line, &msg) == nil && msg.MessageType == "summary" {
			summary = &BackupSummary{}
			_ = json.Unmarshal(line, summary)
		}
	}

	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("restic backup failed: %w", err)
	}

	if summary == nil {
		return nil, fmt.Errorf("restic backup completed but no summary in output")
	}

	return summary, nil
}
