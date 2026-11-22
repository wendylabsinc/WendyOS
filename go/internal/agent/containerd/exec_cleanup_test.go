package containerd

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

type execCleanupProcess struct {
	containerd.Process
	start   func(context.Context) error
	kill    func(context.Context, syscall.Signal, ...containerd.KillOpts) error
	delete  func(context.Context, ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error)
	wait    func(context.Context) (<-chan containerd.ExitStatus, error)
	streams cio.IO
}

func (p *execCleanupProcess) Start(ctx context.Context) error { return p.start(ctx) }
func (p *execCleanupProcess) Kill(ctx context.Context, sig syscall.Signal, opts ...containerd.KillOpts) error {
	return p.kill(ctx, sig, opts...)
}
func (p *execCleanupProcess) Delete(ctx context.Context, opts ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error) {
	return p.delete(ctx, opts...)
}
func (p *execCleanupProcess) Wait(ctx context.Context) (<-chan containerd.ExitStatus, error) {
	return p.wait(ctx)
}
func (p *execCleanupProcess) IO() cio.IO { return p.streams }

func assertExecCleanupContext(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := ctx.Err(); err != nil {
		t.Errorf("cleanup inherited canceled context: %v", err)
	}
	if namespace, ok := namespaces.Namespace(ctx); !ok || namespace != "exec-test" {
		t.Errorf("cleanup lost namespace: %q, %v", namespace, ok)
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Error("cleanup has no deadline")
	}
}

func TestExecCancellationCleansUpWithLiveNamespaceContext(t *testing.T) {
	for _, reportsExit := range []bool{true, false} {
		name := "exit-reported"
		if !reportsExit {
			name = "exit-never-reported"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(namespaces.WithNamespace(context.Background(), "exec-test"))
			defer cancel()
			status := make(chan containerd.ExitStatus, 1)
			var waitCtx context.Context
			killed, deleted := false, false
			proc := &execCleanupProcess{
				wait:  func(ctx context.Context) (<-chan containerd.ExitStatus, error) { waitCtx = ctx; return status, nil },
				start: func(context.Context) error { cancel(); return nil },
				kill: func(ctx context.Context, sig syscall.Signal, opts ...containerd.KillOpts) error {
					assertExecCleanupContext(t, ctx)
					if waitCtx.Err() != nil {
						t.Error("exit observation canceled before killing process")
					}
					var killInfo containerd.KillInfo
					for _, opt := range opts {
						if err := opt(ctx, &killInfo); err != nil {
							t.Fatal(err)
						}
					}
					if sig != syscall.SIGKILL || killInfo.All {
						t.Errorf("kill = %v, all=%v; must target only the owned exec", sig, killInfo.All)
					}
					killed = true
					if reportsExit {
						status <- *containerd.NewExitStatus(137, time.Now(), nil)
					}
					return nil
				},
				delete: func(ctx context.Context, opts ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error) {
					assertExecCleanupContext(t, ctx)
					if !killed {
						t.Error("deleted before attempting kill")
					}
					if len(opts) == 0 {
						t.Error("deletion must retry killing if initial kill failed")
					}
					deleted = true
					return nil, nil
				},
			}
			started := time.Now()
			code, err := runExecProcess(ctx, proc, false, nil, 20*time.Millisecond)
			if code != -1 || !errors.Is(err, context.Canceled) {
				t.Fatalf("got (%d, %v)", code, err)
			}
			if !killed || !deleted {
				t.Fatalf("killed=%v, deleted=%v", killed, deleted)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("cleanup blocked for %v", elapsed)
			}
			if !errors.Is(waitCtx.Err(), context.Canceled) {
				t.Error("exit observer was not released after cleanup")
			}
		})
	}
}

type execCleanupIO struct {
	cio.IO
	waiting chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *execCleanupIO) Wait() { s.once.Do(func() { close(s.waiting) }); <-s.release }
func (s *execCleanupIO) Cancel() {
	select {
	case <-s.release:
	default:
		close(s.release)
	}
}

func TestExecDrainsOutputAndCanCancelDrain(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		name := "drain-all-output"
		if disconnect {
			name = "disconnect-during-drain"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(namespaces.WithNamespace(context.Background(), "exec-test"))
			defer cancel()
			status := make(chan containerd.ExitStatus, 1)
			streams := &execCleanupIO{waiting: make(chan struct{}), release: make(chan struct{})}
			deleted := make(chan struct{})
			proc := &execCleanupProcess{
				streams: streams,
				wait:    func(context.Context) (<-chan containerd.ExitStatus, error) { return status, nil },
				start:   func(context.Context) error { status <- *containerd.NewExitStatus(7, time.Now(), nil); return nil },
				delete: func(ctx context.Context, _ ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error) {
					assertExecCleanupContext(t, ctx)
					close(deleted)
					return nil, nil
				},
			}
			done := make(chan struct{})
			var code int
			var err error
			go func() { code, err = runExecProcess(ctx, proc, false, nil, time.Second); close(done) }()
			<-streams.waiting
			select {
			case <-done:
				t.Fatal("returned before output drained")
			default:
			}
			if disconnect {
				cancel()
			} else {
				close(streams.release)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("output drain did not finish")
			}
			if disconnect {
				if code != -1 || !errors.Is(err, context.Canceled) {
					t.Fatalf("got (%d, %v)", code, err)
				}
			} else if code != 7 || err != nil {
				t.Fatalf("got (%d, %v)", code, err)
			}
			<-deleted
		})
	}
}

func TestExecFailedStartStillDeletesWithFreshContext(t *testing.T) {
	ctx, cancel := context.WithCancel(namespaces.WithNamespace(context.Background(), "exec-test"))
	defer cancel()
	deleted := false
	proc := &execCleanupProcess{
		wait: func(context.Context) (<-chan containerd.ExitStatus, error) {
			return make(chan containerd.ExitStatus), nil
		},
		start: func(context.Context) error { cancel(); return context.Canceled },
		delete: func(ctx context.Context, _ ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error) {
			assertExecCleanupContext(t, ctx)
			deleted = true
			return nil, nil
		},
	}
	code, err := runExecProcess(ctx, proc, false, nil, time.Second)
	if code != -1 || !errors.Is(err, context.Canceled) || !deleted {
		t.Fatalf("got (%d, %v), deleted=%v", code, err, deleted)
	}
}
