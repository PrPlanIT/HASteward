package k8s

import (
	"bytes"
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecResult holds the output of a pod exec command.
type ExecResult struct {
	Stdout string
	Stderr string
}

// ExecCommand runs a command in a container via the Kubernetes exec API.
// If stdin is nil, no stdin is attached. stdout and stderr are captured
// and returned. For streaming, use ExecStream instead.
func ExecCommand(ctx context.Context, pod, namespace, container string, command []string) (*ExecResult, error) {
	c := GetClients()
	if c == nil {
		return nil, fmt.Errorf("kubernetes clients not initialized")
	}

	req := c.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.RestConfig, "POST", req.URL())
	if err != nil {
		return nil, fmt.Errorf("failed to create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return &ExecResult{
			Stdout: stdout.String(),
			Stderr: stderr.String(),
		}, fmt.Errorf("exec failed: %w (stderr: %s)", err, stderr.String())
	}

	return &ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}, nil
}

// ExecStream runs a command in a container with full streaming I/O control.
// The caller provides stdin, stdout, and stderr writers/readers directly.
// Any of stdin, stdout, stderr may be nil.
func ExecStream(ctx context.Context, pod, namespace, container string,
	command []string, stdin io.Reader, stdout, stderr io.Writer) error {

	c := GetClients()
	if c == nil {
		return fmt.Errorf("kubernetes clients not initialized")
	}

	req := c.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     stdin != nil,
			Stdout:    stdout != nil,
			Stderr:    stderr != nil,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.RestConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to create executor: %w", err)
	}

	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	})
}

// ExecCommandWithEnv runs a command in a container, wrapping it in sh -c
// to set environment variables. This avoids exposing secrets in command args
// visible to process listings.
func ExecCommandWithEnv(ctx context.Context, pod, namespace, container string,
	env map[string]string, command []string) (*ExecResult, error) {

	// Build the env var exports and the actual command as a single shell script
	var script bytes.Buffer
	for k, v := range env {
		// Use printf to avoid issues with special characters in values
		fmt.Fprintf(&script, "export %s=\"$(printf '%%s' '%s')\"\n", k, ShellEscape(v))
	}
	for _, arg := range command {
		script.WriteString(arg)
		script.WriteByte(' ')
	}

	return ExecCommand(ctx, pod, namespace, container, []string{"sh", "-c", script.String()})
}

// ShellEscape escapes single quotes for use in a single-quoted shell string.
func ShellEscape(s string) string {
	// Replace ' with '\'' (end quote, escaped quote, start quote)
	result := bytes.ReplaceAll([]byte(s), []byte("'"), []byte("'\\''"))
	return string(result)
}
