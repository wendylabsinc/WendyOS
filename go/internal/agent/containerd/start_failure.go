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

const failedStartOutputLimit = 64 << 10

type failedStartTask interface {
	Delete(context.Context, ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error)
	IO() cio.IO
}

// outputTail retains bounded startup diagnostics while continuing to drain IO.
type outputTail []byte

func (t *outputTail) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= failedStartOutputLimit {
		*t = append((*t)[:0], p[len(p)-failedStartOutputLimit:]...)
	} else {
		if drop := len(*t) + len(p) - failedStartOutputLimit; drop > 0 {
			*t = (*t)[drop:]
		}
		*t = append(*t, p...)
	}
	return n, nil
}

// deleteFailedStart drains the pipes before Delete: containerd Delete waits for
// its FIFO copies, whose writes otherwise block forever on our unread pipes.
// Only failed starts use this path; successful tasks retain their normal output
// stream. Closing IO at the deadline also bounds containerd's context-free Wait.
func deleteFailedStart(ctx context.Context, task failedStartTask, stdoutR, stderrR *io.PipeReader, stdoutW, stderrW *io.PipeWriter, timeout time.Duration) (string, string, error) {
	var stdout, stderr outputTail
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); _, _ = io.Copy(&stdout, stdoutR) }()
	go func() { defer readers.Done(); _, _ = io.Copy(&stderr, stderrR) }()
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
	return string(stdout), string(stderr), err
}

func (c *Client) failTaskStart(ctx context.Context, task failedStartTask, appName string, cause error, stdoutR, stderrR *io.PipeReader, stdoutW, stderrW *io.PipeWriter) error {
	c.logger.Error("Container startup failed", zap.String("app_name", appName), zap.Error(cause))
	stdout, stderr, cleanupErr := deleteFailedStart(ctx, task, stdoutR, stderrR, stdoutW, stderrW, 10*time.Second)
	if stdout != "" || stderr != "" || cleanupErr != nil {
		c.logger.Warn("Failed container startup diagnostics", zap.String("app_name", appName),
			zap.String("stdout_tail", stdout), zap.String("stderr_tail", stderr), zap.NamedError("cleanup_error", cleanupErr))
	}
	c.recordStartFailure(ctx, appName, cause)
	return cause
}
