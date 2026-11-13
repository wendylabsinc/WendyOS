package containerd

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
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
	task := &failureDrainTask{writer: w, payload: strings.Repeat("x", 128<<10) + "startup failure"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		stdout, stderr, err := deleteFailedStart(context.Background(), task, r, er, w, ew, time.Second)
		if err != nil || stderr != 0 || stdout != int64(len(task.payload)) {
			t.Errorf("cleanup: stdout bytes=%d stderr bytes=%d error=%v", stdout, stderr, err)
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
	if err != context.DeadlineExceeded || stderr != int64(len("early stderr")) || time.Since(start) > time.Second {
		t.Fatalf("cleanup stderr bytes=%d err=%v duration=%s", stderr, err, time.Since(start))
	}
}

func TestFailedCNIAddCancellationStillReleasesPartialAllocation(t *testing.T) {
	ctx, cancel := context.WithCancel(namespaces.WithNamespace(context.Background(), "wendy-test"))
	allocated := true // ADD reserved an address before its RPC was canceled.
	cancel()
	err := rollbackCNIAdd(ctx, time.Second, func(delCtx context.Context) error {
		if delCtx.Err() != nil {
			t.Fatal("CNI DEL inherited canceled ADD context")
		}
		if ns, _ := namespaces.Namespace(delCtx); ns != "wendy-test" {
			t.Fatal("cleanup lost its namespace")
		}
		if _, bounded := delCtx.Deadline(); !bounded {
			t.Fatal("cleanup has no deadline")
		}
		allocated = false
		return nil
	})
	if err != nil || allocated {
		t.Fatalf("partial allocation remains=%v error=%v", allocated, err)
	}
}

func TestFailedCNIAddRollbackHasFiniteDeadline(t *testing.T) {
	err := rollbackCNIAdd(context.Background(), 20*time.Millisecond, func(delCtx context.Context) error {
		<-delCtx.Done()
		return delCtx.Err()
	})
	if err != context.DeadlineExceeded {
		t.Fatalf("rollback returned %v", err)
	}
}
