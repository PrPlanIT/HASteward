package restic

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Client wraps the restic binary for repository operations.
type Client struct {
	// Binary is the path to the restic binary (default: "restic").
	Binary string
	// Repository is the restic repository location (path or s3:URL).
	Repository string
	// Password is the repository encryption password.
	Password string
	// Env holds additional environment variables (e.g., AWS_ACCESS_KEY_ID).
	Env map[string]string
}

// NewClient creates a new restic client.
func NewClient(repository, password string) *Client {
	return &Client{
		Repository: repository,
		Password:   password,
	}
}

// binary returns the path to the restic binary.
func (c *Client) binary() string {
	if c.Binary != "" {
		return c.Binary
	}
	return "restic"
}

// buildEnv returns restic-specific environment variables.
func (c *Client) buildEnv() []string {
	env := []string{
		"RESTIC_REPOSITORY=" + c.Repository,
		"RESTIC_PASSWORD=" + c.Password,
	}
	for k, v := range c.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// Run executes a restic command and returns stdout.
func (c *Client) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.binary(), args...)
	cmd.Env = append(os.Environ(), c.buildEnv()...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("restic %s failed: %w\nstderr: %s",
			args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// RunWithStdin executes a restic command with stdin piped from the provided reader.
func (c *Client) RunWithStdin(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.binary(), args...)
	cmd.Env = append(os.Environ(), c.buildEnv()...)
	cmd.Stdin = stdin

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("restic %s failed: %w\nstderr: %s",
			args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// RunWithStdout executes a restic command and writes stdout to the provided writer.
func (c *Client) RunWithStdout(ctx context.Context, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, c.binary(), args...)
	cmd.Env = append(os.Environ(), c.buildEnv()...)
	cmd.Stdout = stdout

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restic %s failed: %w\nstderr: %s",
			args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// BuildTagArgs builds --tag flags for adding tags to a snapshot (backup).
// Each tag is a separate --tag flag.
func BuildTagArgs(tags map[string]string) []string {
	var args []string
	for k, v := range tags {
		args = append(args, "--tag", k+"="+v)
	}
	return args
}

// BuildTagFilter builds a single --tag flag for filtering snapshots (AND semantics).
// Tags are comma-separated within one --tag flag for AND matching.
func BuildTagFilter(tags map[string]string) []string {
	if len(tags) == 0 {
		return nil
	}
	var parts []string
	for k, v := range tags {
		parts = append(parts, k+"="+v)
	}
	return []string{"--tag", strings.Join(parts, ",")}
}
