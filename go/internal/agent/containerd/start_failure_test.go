package containerd

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
)

type failureDrainTask struct {
	containerd.Task
	writer  io.Writer
	payload string
	stall   bool
}

func (f *failureDrainTask) IO() cio.IO { return nil }
func (f *failureDrainTask) Delete(ctx context.Context, _ ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error) {
	// Models the FIFO copy that real containerd waits for before task deletion.
	_, err := io.WriteString(f.writer, f.payload)
	if f.stall {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, err
}

func TestFailedStartDrainsOutputBeforeDelete(t *testing.T) {
	r, w := io.Pipe()
	er, ew := io.Pipe()
	task := &failureDrainTask{writer: w, payload: strings.Repeat("x", failedStartOutputLimit*2) + "startup failure"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		stdout, stderr, err := deleteFailedStart(context.Background(), task, r, er, w, ew, time.Second)
		if err != nil || stderr != "" || len(stdout) != failedStartOutputLimit || !strings.HasSuffix(stdout, "startup failure") {
			t.Errorf("cleanup: stdout bytes=%d stderr=%q error=%v", len(stdout), stderr, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Delete blocked on unread startup output")
	}
}

func TestFailedStartCleanupSurvivesCanceledRPCAndBoundsStall(t *testing.T) {
	r, w := io.Pipe()
	er, ew := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	task := &failureDrainTask{writer: ew, payload: "early stderr", stall: true}
	start := time.Now()
	_, stderr, err := deleteFailedStart(ctx, task, r, er, w, ew, 100*time.Millisecond)
	if err != context.DeadlineExceeded || stderr != "early stderr" || time.Since(start) > time.Second {
		t.Fatalf("cleanup stderr=%q err=%v duration=%s", stderr, err, time.Since(start))
	}
}
