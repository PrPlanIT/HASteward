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

// execImpl is the backend for ExecCommand. Production uses execViaSPDY; tests
// override it via SetExecHookForTest to return canned results, because the SPDY
// executor needs a real API server and cannot be faked by the fake clientset.
var execImpl = execViaSPDY

// SetExecHookForTest replaces the exec backend and returns a restore func.
// TEST-ONLY. The hook receives the FINAL argv — note ExecCommandWithEnv wraps its
// command as `sh -c <script> sh <realcmd...>`, so match on a substring of
// strings.Join(command, " ") (e.g. "grastate.dat", "SELECT 1", "wsrep_recover").
func SetExecHookForTest(fn func(ctx context.Context, pod, namespace, container string, command []string) (*ExecResult, error)) (restore func()) {
	prev := execImpl
	execImpl = fn
	return func() { execImpl = prev }
}

// ExecCommand runs a command in a container via the Kubernetes exec API.
// If stdin is nil, no stdin is attached. stdout and stderr are captured
// and returned. For streaming, use ExecStream instead.
func ExecCommand(ctx context.Context, pod, namespace, container string, command []string) (*ExecResult, error) {
	return execImpl(ctx, pod, namespace, container, command)
}

func execViaSPDY(ctx context.Context, pod, namespace, container string, command []string) (*ExecResult, error) {
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

// podLogsImpl is the backend for PodLogs; tests override it via
// SetPodLogsHookForTest to return canned logs (the fake clientset's GetLogs
// returns a fixed string, not real container output).
var podLogsImpl = podLogsViaAPI

// PodLogs returns a pod's logs as a string, or "" on error.
func PodLogs(ctx context.Context, namespace, pod string) string {
	return podLogsImpl(ctx, namespace, pod)
}

// SetPodLogsHookForTest replaces the pod-logs backend and returns a restore func.
// TEST-ONLY.
func SetPodLogsHookForTest(fn func(ctx context.Context, namespace, pod string) string) (restore func()) {
	prev := podLogsImpl
	podLogsImpl = fn
	return func() { podLogsImpl = prev }
}

func podLogsViaAPI(ctx context.Context, namespace, pod string) string {
	c := GetClients()
	if c == nil {
		return ""
	}
	req := c.Clientset.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		return ""
	}
	defer stream.Close()
	data, _ := io.ReadAll(stream)
	return string(data)
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

// ExecCommandWithEnv runs a command in a container with environment variables.
// Env vars are set via shell exports, but the actual command is passed as
// positional args through exec "$@" — preserving argv exactly without shell
// reparsing. This avoids exposing secrets in command args visible to process
// listings, and prevents shell metacharacters in args (SQL parentheses, quotes)
// from being interpreted.
func ExecCommandWithEnv(ctx context.Context, pod, namespace, container string,
	env map[string]string, command []string) (*ExecResult, error) {

	// Build a minimal shell script that exports env vars then exec's the real command.
	// The real command is passed as positional args ($@), never reparsed by the shell.
	var script bytes.Buffer
	for k, v := range env {
		fmt.Fprintf(&script, "export %s='%s'\n", k, ShellEscape(v))
	}
	script.WriteString("exec \"$@\"")

	// sh -c '<script>' sh <command args...>
	// The "sh" after the script is $0; command args become $1, $2, etc.
	args := []string{"sh", "-c", script.String(), "sh"}
	args = append(args, command...)

	return ExecCommand(ctx, pod, namespace, container, args)
}

// ShellEscape escapes single quotes for use in a single-quoted shell string.
func ShellEscape(s string) string {
	// Replace ' with '\'' (end quote, escaped quote, start quote)
	result := bytes.ReplaceAll([]byte(s), []byte("'"), []byte("'\\''"))
	return string(result)
}
