package containerd

import (
	"context"
	"io"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"go.uber.org/zap"
)

type failedStartTask interface {
	Delete(context.Context, ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error)
	IO() cio.IO
}

// deleteFailedStart drains the pipes before Delete: containerd Delete waits for
// its FIFO copies, whose writes otherwise block forever on our unread pipes.
// Only failed starts use this path; successful tasks retain their normal output
// stream. Closing IO at the deadline also bounds containerd's context-free Wait.
func deleteFailedStart(ctx context.Context, task failedStartTask, stdoutR, stderrR *io.PipeReader, stdoutW, stderrW *io.PipeWriter, timeout time.Duration) (int64, int64, error) {
	var stdoutBytes, stderrBytes int64
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); stdoutBytes, _ = io.Copy(io.Discard, stdoutR) }()
	go func() { defer readers.Done(); stderrBytes, _ = io.Copy(io.Discard, stderrR) }()
	var closeOnce sync.Once
	closePipes := func() {
		closeOnce.Do(func() {
			_ = stdoutR.Close()
			_ = stderrR.Close()
			_ = stdoutW.Close()
			_ = stderrW.Close()
		})
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	stopDeadline := context.AfterFunc(cleanupCtx, func() {
		closePipes()
		if taskIO := task.IO(); taskIO != nil {
			taskIO.Cancel()
			_ = taskIO.Close()
		}
	})
	_, err := task.Delete(cleanupCtx, containerd.WithProcessKill)
	stopDeadline()
	closePipes()
	readers.Wait()
	return stdoutBytes, stderrBytes, err
}

// rollbackCNIAdd must survive the failed ADD's canceled request context.
// Keep namespace values but bound the separate attempt to release partial IPAM.
func rollbackCNIAdd(ctx context.Context, timeout time.Duration, del func(context.Context) error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	return del(cleanupCtx)
}

func (c *Client) failTaskStart(ctx context.Context, task failedStartTask, appName string, cause error, stdoutR, stderrR *io.PipeReader, stdoutW, stderrW *io.PipeWriter) error {
	c.logger.Error("Container startup failed", zap.String("app_name", appName), zap.Error(cause))
	stdoutBytes, stderrBytes, cleanupErr := deleteFailedStart(ctx, task, stdoutR, stderrR, stdoutW, stderrW, 10*time.Second)
	// Application output can contain credentials. Drain it without copying its
	// content into the shared agent logger or retaining a diagnostic tail.
	if stdoutBytes != 0 || stderrBytes != 0 || cleanupErr != nil {
		c.logger.Warn("Failed container startup diagnostics", zap.String("app_name", appName),
			zap.Int64("stdout_bytes", stdoutBytes), zap.Int64("stderr_bytes", stderrBytes), zap.NamedError("cleanup_error", cleanupErr))
	}
	c.recordStartFailure(ctx, appName, cause)
	return cause
}
