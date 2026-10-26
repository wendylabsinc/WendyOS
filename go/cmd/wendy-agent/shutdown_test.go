package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"
)

// openStream starts a real gRPC server with one never-ending server stream
// open on it: a health Watch, which, like a followed log, only ends when its
// client goes away. It returns the server and the client's stream errors.
func openStream(t *testing.T) (*grpc.Server, <-chan error) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	stream, err := healthpb.NewHealthClient(conn).Watch(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil { // the stream is open once the first status arrives
		t.Fatal(err)
	}
	streamErr := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				streamErr <- err
				return
			}
		}
	}()
	return srv, streamErr
}

func TestGracefulStopAloneHangsOnAnOpenStream(t *testing.T) {
	// The bug this shutdown fixes: a connected client kept the agent alive
	// until systemd killed it 90 s later.
	srv, _ := openStream(t)
	done := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("GracefulStop returned with a stream open; the premise of stopGRPCServer no longer holds")
	case <-time.After(300 * time.Millisecond):
	}
	srv.Stop()
	<-done
}

func TestStopGRPCServerCancelsStreamsThatNeverEnd(t *testing.T) {
	srv, streamErr := openStream(t)

	start := time.Now()
	if stopGRPCServer(srv, 100*time.Millisecond) {
		t.Error("stopGRPCServer() reported a clean drain with a stream still open")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("stopGRPCServer() took %v with a 100ms drain", elapsed)
	}
	select {
	case err := <-streamErr:
		if err == nil {
			t.Error("the stream ended without an error")
		}
	case <-time.After(2 * time.Second):
		t.Error("the open stream was never cancelled")
	}
}

// fakeServer drains at once, or never until Stop is called.
type fakeServer struct {
	hang    bool
	stopped chan struct{}
	once    sync.Once
	stops   atomic.Int32
}

func newFakeServer(hang bool) *fakeServer {
	return &fakeServer{hang: hang, stopped: make(chan struct{})}
}

func (f *fakeServer) GracefulStop() {
	if f.hang {
		<-f.stopped
	}
}

func (f *fakeServer) Stop() {
	f.stops.Add(1)
	f.once.Do(func() { close(f.stopped) })
}

func TestStopGRPCServerReturnsAsSoonAsDrained(t *testing.T) {
	srv := newFakeServer(false)
	if !stopGRPCServer(srv, time.Minute) {
		t.Error("stopGRPCServer() = false for a server that drained")
	}
	if srv.stops.Load() != 0 {
		t.Error("a drained server was force-stopped")
	}
}

// shutdownRecorder records the order shutdown steps ran in.
type shutdownRecorder struct {
	mu             sync.Mutex
	steps          []string
	appCtxDeadline bool
}

func (r *shutdownRecorder) add(step string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step)
}

func (r *shutdownRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.steps, ",")
}

// recordingServer drains at once and records that it has.
type recordingServer struct{ rec *shutdownRecorder }

func (s recordingServer) GracefulStop() { s.rec.add("servers stopped") }
func (s recordingServer) Stop()         {}

func newRecordedShutdown(rec *shutdownRecorder, hostDown bool) *agentShutdown {
	return &agentShutdown{
		logger:           zap.NewNop(),
		hostShuttingDown: func(context.Context) bool { return hostDown },
		cancel:           func() { rec.add("cancel") },
		servers:          []namedGRPCServer{{"agent", recordingServer{rec}}},
		stopApps: func(ctx context.Context) (int, error) {
			_, ok := ctx.Deadline()
			rec.mu.Lock()
			rec.appCtxDeadline = ok
			rec.mu.Unlock()
			rec.add("stop apps")
			return 3, nil
		},
	}
}

func TestShutdownStopsAppsWhenTheHostIsGoingDown(t *testing.T) {
	rec := &shutdownRecorder{}
	newRecordedShutdown(rec, true).run()
	// The restart monitor must be stopped before the apps, or it would start
	// them again. Apps are stopped while the servers drain, then once more
	// after the servers have stopped, for any app an RPC started meanwhile.
	switch got := rec.String(); got {
	case "cancel,stop apps,servers stopped,stop apps", "cancel,servers stopped,stop apps,stop apps":
	default:
		t.Errorf("steps = %q, want cancel, stop apps alongside the servers, then stop apps again", got)
	}
	if !rec.appCtxDeadline {
		t.Error("stopping apps was not given a deadline")
	}
}

func TestShutdownLeavesAppsRunningWhenOnlyTheAgentRestarts(t *testing.T) {
	// An agent update or `systemctl restart` must not take the apps down:
	// the next agent adopts them.
	rec := &shutdownRecorder{}
	newRecordedShutdown(rec, false).run()
	if got := rec.String(); got != "cancel,servers stopped" {
		t.Errorf("steps = %q, want the apps left running", got)
	}
}

func TestShutdownIsBoundedWhenStreamsAndLoopsHang(t *testing.T) {
	hung, drained := newFakeServer(true), newFakeServer(false)
	var background sync.WaitGroup
	background.Add(1) // a loop that never notices the cancellation
	t.Cleanup(background.Done)

	s := &agentShutdown{
		logger:                zap.NewNop(),
		cancel:                func() {},
		servers:               []namedGRPCServer{{"agent", hung}, {"OTEL", drained}},
		shutdownHTTP:          func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		background:            &background,
		grpcDrainTimeout:      50 * time.Millisecond,
		backgroundStopTimeout: 50 * time.Millisecond,
	}
	start := time.Now()
	s.run()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("shutdown took %v with every step stuck; want about drain + background timeouts", elapsed)
	}
	if hung.stops.Load() == 0 {
		t.Error("the server with an open stream was never force-stopped")
	}
	if drained.stops.Load() != 0 {
		t.Error("a server that drained was force-stopped")
	}
}

func TestShutdownWatchdogExitsWhenShutdownHangs(t *testing.T) {
	exited := make(chan int, 1)
	armShutdownWatchdog(zap.NewNop(), 20*time.Millisecond, func(code int) { exited <- code })
	select {
	case code := <-exited:
		if code == 0 {
			t.Error("a forced exit reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the watchdog never fired")
	}

	disarm := armShutdownWatchdog(zap.NewNop(), 50*time.Millisecond, func(code int) { exited <- code })
	disarm()
	select {
	case <-exited:
		t.Error("a disarmed watchdog fired")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestHostShuttingDown(t *testing.T) {
	notFound := errors.New(`exec: "systemctl": executable file not found in $PATH`)
	exitStatus := errors.New("exit status 1")
	for _, tc := range []struct {
		name  string
		state string
		jobs  string
		err   error
		want  bool
	}{
		{name: "powering off", state: "stopping\n", err: exitStatus, want: true},
		{name: "running", state: "running\n", want: false},
		{name: "degraded but up", state: "degraded\n", err: exitStatus, want: false},
		{
			name: "shut down before boot finished", state: "starting\n", err: exitStatus,
			jobs: "123 shutdown.target start waiting\n124 poweroff.target start waiting\n", want: true,
		},
		{name: "still booting", state: "starting\n", err: exitStatus, jobs: "", want: false},
		{name: "no systemd", err: notFound, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			systemctl := func(_ context.Context, args ...string) (string, error) {
				switch args[0] {
				case "is-system-running":
					return tc.state, tc.err
				case "list-jobs":
					if errors.Is(tc.err, notFound) {
						return "", tc.err
					}
					return tc.jobs, nil
				}
				t.Fatalf("unexpected systemctl %v", args)
				return "", nil
			}
			if got := hostShuttingDown(context.Background(), systemctl); got != tc.want {
				t.Errorf("hostShuttingDown() = %v, want %v", got, tc.want)
			}
		})
	}
}
