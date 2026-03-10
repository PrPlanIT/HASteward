package k8s

import (
	"context"
	"io"
	"os"
	"sync/atomic"
)

// progressWriter wraps a writer and calls a callback with running byte count.
type progressWriter struct {
	w        io.Writer
	written  atomic.Int64
	callback func(int64)
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.w.Write(p)
	if n > 0 {
		total := pw.written.Add(int64(n))
		if pw.callback != nil {
			pw.callback(total)
		}
	}
	return n, err
}

// progressReader wraps a reader and calls a callback with running byte count.
type progressReader struct {
	r        io.Reader
	read     atomic.Int64
	callback func(int64)
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	if n > 0 {
		total := pr.read.Add(int64(n))
		if pr.callback != nil {
			pr.callback(total)
		}
	}
	return n, err
}

// ExecPipeOut runs a command in a pod and returns a ReadCloser for stdout.
// The exec runs in a background goroutine. Call the returned wait function
// after fully consuming the reader to get any exec error.
func ExecPipeOut(ctx context.Context, pod, namespace, container string,
	command []string) (io.ReadCloser, func() error) {

	pr, pw := io.Pipe()

	var execErr error
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer pw.Close()
		execErr = ExecStream(ctx, pod, namespace, container, command, nil, pw, os.Stderr)
		if execErr != nil {
			pw.CloseWithError(execErr)
		}
	}()

	return pr, func() error {
		<-done
		return execErr
	}
}
