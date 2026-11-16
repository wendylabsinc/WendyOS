package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// waitRestartExit returns the exit code an agent restart asked for.
func waitRestartExit(t *testing.T, exited <-chan int) int {
	t.Helper()
	select {
	case code := <-exited:
		return code
	case <-time.After(5 * time.Second):
		t.Fatal("the agent never exited to restart")
		return -1
	}
}

// The agent as main wires it: Wendy Net running under the context SIGTERM
// cancels, and the termination path waiting on that context. A restart stops
// Wendy Net and exits; it never takes the termination path, the only one that
// can stop apps, not even on a host that says it is shutting down.
func TestAgentRestartStopsWendyNetAndLeavesAppsRunning(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mesh meshLifetime
	var drained atomic.Int32
	mesh.Start(ctx, func(ctx context.Context, launch func(func())) {
		for range 3 {
			// A worker returns once its links, rules and routes are removed.
			launch(func() { <-ctx.Done(); time.Sleep(10 * time.Millisecond); drained.Add(1) })
		}
	})
	var appStops atomic.Int32
	terminated := make(chan struct{})
	go func() {
		defer close(terminated)
		<-ctx.Done()
		(&agentShutdown{
			logger:           zap.NewNop(),
			hostShuttingDown: func(context.Context) bool { return true },
			cancel:           cancel,
			stopApps:         func(context.Context) (int, error) { appStops.Add(1); return 0, nil },
		}).run()
	}()

	exited := make(chan int, 1)
	var drainedAtExit int32
	restart := newAgentRestart(logger, &mesh)
	restart.AckDelay = time.Millisecond
	restart.Exit = func(code int) { drainedAtExit = drained.Load(); exited <- code }
	restart.Schedule()

	if code := waitRestartExit(t, exited); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if drainedAtExit != 3 {
		t.Fatalf("the agent exited with %d of 3 Wendy Net workers drained", drainedAtExit)
	}
	if ctx.Err() != nil {
		t.Fatal("the restart cancelled the agent's main context, which starts the termination path")
	}
	// Long enough for the termination path to have run, had it been started.
	time.Sleep(50 * time.Millisecond)
	if n := appStops.Load(); n != 0 {
		t.Fatalf("the restart stopped apps (%d sweeps); apps keep running across an agent restart", n)
	}
	if n := logs.FilterMessage("Stopped Wendy Net before the agent restart").Len(); n != 1 {
		t.Fatalf("logged the Wendy Net stop %d times, want once", n)
	}

	// The same wiring does stop apps once the agent is told to terminate, so
	// the count above was not zero for want of a path to them.
	cancel()
	<-terminated
	if appStops.Load() == 0 {
		t.Fatal("the termination path of this test never reached the apps")
	}
}

// On a host that never started Wendy Net the restart has nothing to stop: it
// exits as soon as the reply delay is over, and says nothing.
func TestAgentRestartWithoutWendyNetHasNothingToStop(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	var mesh meshLifetime

	exited := make(chan int, 1)
	restart := newAgentRestart(zap.New(core), &mesh)
	restart.AckDelay = time.Millisecond
	// Far longer than the test: the stop step must not wait for it.
	restart.StopTimeout = time.Hour
	restart.Exit = func(code int) { exited <- code }
	restart.Schedule()

	if code := waitRestartExit(t, exited); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if logs.Len() != 0 {
		t.Fatalf("logged %v on a host without Wendy Net", logs.All())
	}
	// Enrollment finishing during the restart must not start a mesh the
	// exit would then leave in the kernel.
	mesh.Start(context.Background(), func(context.Context, func(func())) {
		t.Error("Wendy Net started while the agent was restarting")
	})
}

// A Wendy Net that does not stop in time must not keep the old agent running.
func TestAgentRestartExitsWhenWendyNetDoesNotStop(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	var mesh meshLifetime
	release := make(chan struct{})
	defer close(release)
	mesh.Start(context.Background(), func(_ context.Context, launch func(func())) {
		launch(func() { <-release })
	})

	exited := make(chan int, 1)
	restart := newAgentRestart(zap.New(core), &mesh)
	restart.AckDelay = time.Millisecond
	restart.StopTimeout = 30 * time.Millisecond
	restart.Exit = func(code int) { exited <- code }
	restart.Schedule()

	if code := waitRestartExit(t, exited); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if logs.Len() != 1 {
		t.Fatalf("want one warning that Wendy Net was left behind, got %v", logs.All())
	}
}
