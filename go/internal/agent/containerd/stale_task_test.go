package containerd

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/errdefs"
)

// Embed the unused interface methods so these fakes exercise the real
// containerd.Container and containerd.Task boundary without a daemon.
type staleContainerForTest struct {
	containerd.Container
	taskFn func(context.Context) (containerd.Task, error)
}

func (f staleContainerForTest) Task(ctx context.Context, _ cio.Attach) (containerd.Task, error) {
	return f.taskFn(ctx)
}

type staleTaskForTest struct {
	containerd.Task
	killFn   func(context.Context) error
	deleteFn func(context.Context) error
}

func (f staleTaskForTest) Wait(context.Context) (<-chan containerd.ExitStatus, error) {
	ch := make(chan containerd.ExitStatus)
	close(ch)
	return ch, nil
}

func (f staleTaskForTest) Kill(ctx context.Context, _ syscall.Signal, _ ...containerd.KillOpts) error {
	if f.killFn != nil {
		return f.killFn(ctx)
	}
	return nil
}

func (f staleTaskForTest) Delete(ctx context.Context, _ ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error) {
	if f.deleteFn != nil {
		return nil, f.deleteFn(ctx)
	}
	return nil, nil
}

func TestDeleteStaleTaskBoundedContainerdRPCs(t *testing.T) {
	// A STOPPED task's shim can hang during lookup, Kill, or Delete. Every
	// stage must honor the same deadline and return a start error to retry.
	for _, blocked := range []string{"lookup", "kill", "delete"} {
		t.Run(blocked, func(t *testing.T) {
			var reached bool
			task := staleTaskForTest{
				killFn: func(ctx context.Context) error {
					if blocked == "kill" {
						reached = true
						<-ctx.Done()
						return ctx.Err()
					}
					return nil
				},
				deleteFn: func(ctx context.Context) error {
					if blocked == "delete" {
						reached = true
						<-ctx.Done()
						return ctx.Err()
					}
					return nil
				},
			}
			container := staleContainerForTest{taskFn: func(ctx context.Context) (containerd.Task, error) {
				if blocked == "lookup" {
					reached = true
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return task, nil
			}}
			started := time.Now()
			err := newTeardownTestClient().deleteStaleTaskWithTimeout(context.Background(), container, "app", 30*time.Millisecond)
			if !reached {
				t.Fatalf("blocked %s stage was not reached", blocked)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want deadline exceeded", err)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("cleanup took %v, want bounded by RPC deadline", elapsed)
			}
		})
	}
}

func TestDeleteStaleTaskPreservesOrphanRepair(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"no task", errdefs.ErrNotFound},
		{"task cannot be loaded", errors.New("runtime orphan")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			container := staleContainerForTest{taskFn: func(context.Context) (containerd.Task, error) {
				return nil, tc.err
			}}
			if err := newTeardownTestClient().deleteStaleTaskWithTimeout(context.Background(), container, "app", time.Second); err != nil {
				t.Fatalf("lookup error should leave NewTask/AlreadyExists repair available: %v", err)
			}
		})
	}
}

func TestDeleteStaleTaskPropagatesLookupDeadline(t *testing.T) {
	container := staleContainerForTest{taskFn: func(context.Context) (containerd.Task, error) {
		return nil, context.DeadlineExceeded
	}}
	err := newTeardownTestClient().deleteStaleTaskWithTimeout(context.Background(), container, "app", time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want lookup deadline exceeded", err)
	}
}

func TestTerminateTaskCanceledDuringWaitSkipsDelete(t *testing.T) {
	task := newFakeTeardownTask() // Kill does not close the wait channel.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := newTeardownTestClient().terminateTask(ctx, task, "app", syscall.SIGKILL, time.Minute, time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if task.deleted {
		t.Fatal("Delete called after cleanup context expired")
	}
}
