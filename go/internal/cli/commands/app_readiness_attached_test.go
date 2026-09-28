package commands

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

// untilStoppedStream acknowledges Started, then keeps streaming output until
// stopped is closed, then ends with io.EOF — the way the agent's stream ends
// when the app is stopped.
type untilStoppedStream struct {
	grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse]
	stopped chan struct{}
	started bool
}

func (s *untilStoppedStream) Recv() (*agentpb.RunContainerLayersResponse, error) {
	if !s.started {
		s.started = true
		return &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_Started_{Started: &agentpb.RunContainerLayersResponse_Started{}}}, nil
	}
	select {
	case <-s.stopped:
		return nil, io.EOF
	case <-time.After(20 * time.Millisecond):
		return &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: []byte("still booting\n")}}}, nil
	}
}

// followTelemetry is a log stream for followExistingContainer that delivers
// one empty batch and then stays open until the follow ends.
func followTelemetry() *runLogsFakeClient {
	return &runLogsFakeClient{stream: &runLogsFakeStream{response: testRunLogsResponse(false, &logspb.ScopeLogs{}), delivered: make(chan struct{})}}
}

// listHook calls after(n) once its n-th ListContainers call (from 1) has its
// answer, so a test can act between two polls.
type listHook struct {
	*scriptedContainerClient
	calls atomic.Int32
	after func(n int32)
}

func (c *listHook) ListContainers(ctx context.Context, req *agentpb.ListContainersRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.ListContainersResponse], error) {
	stream, err := c.scriptedContainerClient.ListContainers(ctx, req, opts...)
	c.after(c.calls.Add(1))
	return stream, err
}

// logsEndAfter is a telemetry client whose log stream ends (io.EOF) once
// after is closed, which makes followExistingContainer poll at once.
type logsEndAfter struct {
	agentpb.WendyTelemetryServiceClient
	after <-chan struct{}
}

func (c *logsEndAfter) StreamLogs(context.Context, *agentpb.StreamLogsRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.StreamLogsResponse], error) {
	return &logsEndAfterStream{after: c.after}, nil
}

type logsEndAfterStream struct {
	grpc.ServerStreamingClient[agentpb.StreamLogsResponse]
	after <-chan struct{}
}

func (s *logsEndAfterStream) Recv() (*agentpb.StreamLogsResponse, error) {
	<-s.after
	return nil, io.EOF
}

// Attached --wait-ready (PR #1882's semantics): a readiness failure fails the
// run instead of the historical warning, and stops the app when this run
// started it. A follow of an app it did not start fails without stopping it.
func TestAttachedWaitReadyStopsAnAppThatNeverBecomesReady(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := testPort(t, ln)
	ln.Close() // nothing listens: the probe can never pass
	cfg := &appconfig.AppConfig{AppID: "app", Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: port}}}
	opts := runOptions{waitReady: true, readinessTimeout: time.Second}

	for _, path := range []string{"chunk-diff", "registry", "follow"} {
		t.Run(path, func(t *testing.T) {
			stream := &untilStoppedStream{stopped: make(chan struct{})}
			fake := &scriptedContainerClient{snapshots: []*agentpb.AppContainer{runningSnapshot(0)}, stream: stream}
			fake.onStop = func() { close(stream.stopped) }
			conn := &grpcclient.AgentConnection{Host: "127.0.0.1", ContainerService: fake, AgentService: &fakeAgentVersionClient{err: errors.New("no version")}}
			switch path {
			case "chunk-diff":
				err = streamRunContainerWithStarted(context.Background(), conn, stream, cfg, opts, nil)
			case "registry":
				err = startExistingContainer(context.Background(), conn, cfg, opts)
			case "follow":
				// The followed app keeps running: the run must end on the
				// gate's failure (before this guard cancels it), not wait
				// for the app to stop, and must not stop an app it did not
				// start.
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				ctx, notes := withInterruptNotes(ctx)
				conn.TelemetryService = followTelemetry()
				err = followExistingContainer(ctx, conn, cfg, opts, appBaseline{})
				if got := fake.stops(); len(got) != 0 {
					t.Fatalf("StopContainer calls = %v, want none: this run did not start the app", got)
				}
				if notes.outcome != interruptedAppLeftRunning {
					t.Fatalf("notes = %+v, want left running", notes)
				}
			}
			if ErrorClass(err) != "readiness_timeout" {
				t.Fatalf("err = %v, want readiness_timeout", err)
			}
			if got := fake.stops(); path != "follow" && (len(got) != 1 || got[0] != "app") {
				t.Fatalf("StopContainer calls = %v, want [app]", got)
			}
		})
	}
}

// A pass runs the host-side postStart work once, without a second probe.
func TestAttachedWaitReadyRunsHooksOnceReady(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var opens atomic.Int32
	opened := make(chan struct{}, 1)
	previous := browserOpen
	t.Cleanup(func() { browserOpen = previous })
	browserOpen = func(string) error {
		opens.Add(1)
		select {
		case opened <- struct{}{}:
		default:
		}
		return nil
	}
	cfg := &appconfig.AppConfig{AppID: "app", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: testPort(t, ln)}}}
	stream := &untilStoppedStream{stopped: make(chan struct{})}
	fake := &scriptedContainerClient{snapshots: []*agentpb.AppContainer{runningSnapshot(0)}}
	conn := &grpcclient.AgentConnection{Host: "127.0.0.1", ContainerService: fake, AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
		NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"127.0.0.1"}}},
	}}}
	go func() {
		select {
		case <-opened:
		case <-time.After(5 * time.Second):
		}
		close(stream.stopped) // the app exits once the browser opened
	}()
	var runErr error
	out := captureStderr(t, func() {
		runErr = streamRunContainerWithStarted(context.Background(), conn, stream, cfg, runOptions{waitReady: true}, nil)
	})
	if runErr != nil {
		t.Fatalf("err = %v, want nil", runErr)
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("browser opened %d times, want exactly once after readiness", got)
	}
	if strings.Contains(out, "to be ready...") {
		t.Fatalf("the hook runner probed again after the gate passed:\n%s", out)
	}
}

// An app whose output ends on its own before it became ready exited: the gate,
// not the stream's end, decides the outcome, and a clean exit (code 0) inside
// the wait fails --wait-ready. The ended stream must not cancel the
// gate's last poll. Attached runs never add the JSON object to stdout.
func TestAttachedWaitReadyCleanExitBeforeReadyFails(t *testing.T) {
	previous := jsonOutput
	t.Cleanup(func() { jsonOutput = previous })
	jsonOutput = true
	cfg := &appconfig.AppConfig{AppID: "app"}
	opts := runOptions{waitReady: true, readinessTimeout: 3 * time.Second}

	for _, path := range []string{"chunk-diff", "registry", "follow"} {
		t.Run(path, func(t *testing.T) {
			stream := &deploymentAckStream{remaining: 1, err: io.EOF} // the app exited on its own
			fake := &scriptedContainerClient{
				snapshots: []*agentpb.AppContainer{runningSnapshot(0), appSnapshot("app", agentpb.AppRunningState_STOPPED, 0, "exited", 0)},
				stream:    stream,
			}
			conn := &grpcclient.AgentConnection{Host: "127.0.0.1", ContainerService: fake}
			var err error
			stdout := captureStdout(t, func() {
				switch path {
				case "chunk-diff":
					err = streamRunContainerWithStarted(context.Background(), conn, stream, cfg, opts, nil)
				case "registry":
					err = startExistingContainer(context.Background(), conn, cfg, opts)
				case "follow":
					// The app's logs end right after the gate's first poll,
					// so the follow sees the exit (second poll) before the
					// gate's next poll can.
					listed := make(chan struct{})
					conn.ContainerService = &listHook{scriptedContainerClient: fake, after: func(n int32) {
						if n == 1 {
							close(listed)
						}
					}}
					conn.TelemetryService = &logsEndAfter{after: listed}
					err = followExistingContainer(context.Background(), conn, cfg, opts, appBaseline{})
				}
			})
			if ErrorClass(err) != "app_crashed" {
				t.Fatalf("err = %v, want app_crashed", err)
			}
			if !strings.Contains(err.Error(), "exited before becoming ready (exit code 0)") {
				t.Fatalf("error %q does not report the clean exit before ready", err)
			}
			if stdout != "" {
				t.Fatalf("attached --wait-ready wrote %q to stdout", stdout)
			}
			// A run that started the app stops it; a follow leaves it alone.
			wantStops := 1
			if path == "follow" {
				wantStops = 0
			}
			if got := fake.stops(); len(got) != wantStops {
				t.Fatalf("StopContainer calls = %v, want %d", got, wantStops)
			}
		})
	}
}

// A crash before ready is reported by the gate, which stops the app (so its
// restart policy cannot bring it back behind a failed run).
func TestAttachedWaitReadyCrashBeforeReadyFailsViaTheGate(t *testing.T) {
	for _, path := range []string{"chunk-diff", "registry"} {
		t.Run(path, func(t *testing.T) {
			stream := &deploymentAckStream{remaining: 1, err: io.EOF}
			fake := &scriptedContainerClient{
				snapshots: []*agentpb.AppContainer{runningSnapshot(0), appSnapshot("app", agentpb.AppRunningState_STOPPED, 3, "crashed", 0)},
				stream:    stream,
			}
			conn := &grpcclient.AgentConnection{Host: "127.0.0.1", ContainerService: fake}
			cfg, opts := &appconfig.AppConfig{AppID: "app"}, runOptions{waitReady: true, readinessTimeout: 3 * time.Second}
			var err error
			out := captureStderr(t, func() {
				if path == "chunk-diff" {
					err = streamRunContainerWithStarted(context.Background(), conn, stream, cfg, opts, nil)
				} else {
					err = startExistingContainer(context.Background(), conn, cfg, opts)
				}
			})
			if ErrorClass(err) != "app_crashed" {
				t.Fatalf("err = %v, want app_crashed", err)
			}
			if !strings.Contains(err.Error(), "exit code 3") {
				t.Fatalf("error %q does not name the exit code", err)
			}
			if got := fake.stops(); len(got) != 1 || got[0] != "app" {
				t.Fatalf("StopContainer calls = %v, want [app] (the gate's stop)", got)
			}
			if strings.Contains(out, "replaced") {
				t.Fatalf("a crash reported as a replacement:\n%s", out)
			}
		})
	}
}

// Another deployment can replace the app while an attached --wait-ready run
// checks it: the replace kills this run's task, which ends the run's output,
// and starts a fresh app. Whatever the gate then sees — the killed task's
// SIGKILL record, the fresh app not listening yet, or a record on another
// app_version — the app running now is the other deployment's: the gate must
// not stop it, and the run reports the replacement once and succeeds.
func TestAttachedWaitReadyLeavesAReplacementRunning(t *testing.T) {
	shortenReplaceConfirm(t, sigkillTestWindow)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := testPort(t, closed)
	closed.Close() // nothing listens: the probe can never pass
	withVersion := func(c *agentpb.AppContainer, version string) *agentpb.AppContainer {
		c.AppVersion = version
		return c
	}
	for _, tc := range []struct {
		name      string
		cfg       *appconfig.AppConfig
		timeout   time.Duration
		snapshots []*agentpb.AppContainer
	}{
		{name: "SIGKILL record, then a fresh app", cfg: &appconfig.AppConfig{AppID: "app"}, timeout: 3 * time.Second, snapshots: []*agentpb.AppContainer{
			runningSnapshot(0),
			appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "crashed", 0),
			runningSnapshot(0),
		}},
		{name: "crash-looping SIGKILL record, then a fresh app", cfg: &appconfig.AppConfig{AppID: "app"}, timeout: 3 * time.Second, snapshots: []*agentpb.AppContainer{
			runningSnapshot(0),
			appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 137, "crashed", 1),
			runningSnapshot(0),
		}},
		{
			name:      "readiness timeout once this run's output ended",
			cfg:       &appconfig.AppConfig{AppID: "app", Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: closedPort}}},
			timeout:   time.Second,
			snapshots: []*agentpb.AppContainer{runningSnapshot(0)},
		},
		{name: "another version's crash", cfg: &appconfig.AppConfig{AppID: "app", Version: "1.0"}, timeout: 3 * time.Second, snapshots: []*agentpb.AppContainer{
			withVersion(runningSnapshot(0), "1.0"),
			withVersion(appSnapshot("app", agentpb.AppRunningState_STOPPED, 3, "crashed", 0), "2.0"),
		}},
	} {
		for _, path := range []string{"chunk-diff", "registry"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				stream := &deploymentAckStream{remaining: 1, err: io.EOF} // the replace ended this run's output
				fake := &scriptedContainerClient{snapshots: tc.snapshots, stream: stream}
				conn := &grpcclient.AgentConnection{Host: "127.0.0.1", ContainerService: fake, AgentService: &fakeAgentVersionClient{err: errors.New("no version")}}
				opts := runOptions{waitReady: true, readinessTimeout: tc.timeout}
				var runErr error
				out := captureStderr(t, func() {
					if path == "chunk-diff" {
						runErr = streamRunContainerWithStarted(context.Background(), conn, stream, tc.cfg, opts, nil)
					} else {
						runErr = startExistingContainer(context.Background(), conn, tc.cfg, opts)
					}
				})
				if runErr != nil {
					t.Fatalf("err = %v, want nil: the app was replaced", runErr)
				}
				if got := fake.stops(); len(got) != 0 {
					t.Fatalf("StopContainer calls = %v, want none: the running app is another deployment's", got)
				}
				if n := strings.Count(out, "Application app was replaced by another deployment."); n != 1 {
					t.Fatalf("replaced notice printed %d times, want once:\n%s", n, out)
				}
				if strings.Contains(out, "did not become ready") || strings.Contains(out, "stopped.") {
					t.Fatalf("a replacement reported as this run's failure or stop:\n%s", out)
				}
			})
		}
	}
}

// A follow's gate that sees the followed app replaced reports the
// replacement, not a crash: the SIGKILL record is judged against the follow's
// baseline, and the app running now is left alone.
func TestFollowWaitReadyReportsAReplacement(t *testing.T) {
	shortenReplaceConfirm(t, sigkillTestWindow)
	// Poll 1 is the gate's baseline; the gate's and the follow's first polls
	// both see the killed task; the fresh app follows.
	fake := &scriptedContainerClient{snapshots: []*agentpb.AppContainer{
		runningSnapshot(2),
		appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 137, "crashed", 3),
		appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 137, "crashed", 3),
		runningSnapshot(0),
	}}
	conn := &grpcclient.AgentConnection{Host: "127.0.0.1", ContainerService: fake, TelemetryService: followTelemetry()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var err error
	out := captureStderr(t, func() {
		err = followExistingContainer(ctx, conn, &appconfig.AppConfig{AppID: "app"}, runOptions{waitReady: true, readinessTimeout: 5 * time.Second}, appBaseline{failures: 2})
	})
	if err != nil {
		t.Fatalf("err = %v, want nil: the app was replaced", err)
	}
	if got := fake.stops(); len(got) != 0 {
		t.Fatalf("StopContainer calls = %v, want none", got)
	}
	if n := strings.Count(out, "Application app was replaced by another deployment."); n != 1 {
		t.Fatalf("replaced notice printed %d times, want once:\n%s", n, out)
	}
}

// Ctrl-C while the gate is still waiting ends the run as a cancellation: the
// gate stands down without stopping anything, so an attached run stops its
// app exactly once and a follow leaves the app it did not start running.
func TestAttachedWaitReadyInterruptStopsTheAppOnce(t *testing.T) {
	cfg := &appconfig.AppConfig{AppID: "app"}
	opts := runOptions{waitReady: true, readinessTimeout: 30 * time.Second}

	for _, path := range []string{"chunk-diff", "registry", "follow"} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx, notes := withInterruptNotes(ctx)
			stream := &interruptOnSecondRecvStream{ctx: ctx, interrupt: cancel}
			fake := &scriptedContainerClient{snapshots: []*agentpb.AppContainer{runningSnapshot(0)}, stream: stream}
			conn := &grpcclient.AgentConnection{Host: "127.0.0.1", ContainerService: fake}
			var err error
			wantStops, wantOutcome := []string{"app"}, interruptedAppStopped
			switch path {
			case "chunk-diff":
				err = streamRunContainerWithStarted(ctx, conn, stream, cfg, opts, nil)
			case "registry":
				err = startExistingContainer(ctx, conn, cfg, opts)
			case "follow":
				telemetry := followTelemetry()
				go func() {
					<-telemetry.stream.delivered
					cancel()
				}()
				conn.TelemetryService = telemetry
				err = followExistingContainer(ctx, conn, cfg, opts, appBaseline{})
				wantStops, wantOutcome = nil, interruptedAppLeftRunning
			}
			if !errors.Is(err, ErrUserCancelled) {
				t.Fatalf("err = %v, want ErrUserCancelled", err)
			}
			if got := fake.stops(); len(got) != len(wantStops) || (len(got) == 1 && got[0] != wantStops[0]) {
				t.Fatalf("StopContainer calls = %v, want %v", got, wantStops)
			}
			if notes.outcome != wantOutcome {
				t.Fatalf("notes = %+v, want outcome %v", notes, wantOutcome)
			}
		})
	}
}

// Ctrl-C while the run waits for the gate's verdict after the app's output
// ended is still a cancellation (ErrUserCancelled): never a nil "stopped"
// success. A run that started the app stops it; a follow leaves it running.
func TestAttachedWaitReadyInterruptWhileAwaitingTheGate(t *testing.T) {
	cfg := &appconfig.AppConfig{AppID: "app"}
	opts := runOptions{waitReady: true, readinessTimeout: 30 * time.Second}

	for _, path := range []string{"chunk-diff", "follow"} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx, notes := withInterruptNotes(ctx)
			stream := &deploymentAckStream{remaining: 1, err: io.EOF} // the output ends at once
			fake := &scriptedContainerClient{
				snapshots: []*agentpb.AppContainer{runningSnapshot(0), appSnapshot("app", agentpb.AppRunningState_STOPPED, 0, "exited", 0)},
				stream:    stream,
			}
			conn := &grpcclient.AgentConnection{Host: "127.0.0.1", ContainerService: fake}
			var err error
			switch path {
			case "chunk-diff":
				// Poll 1 is the gate's baseline, poll 2 its first tick, a
				// second after the stream ended: interrupt then.
				conn.ContainerService = &listHook{scriptedContainerClient: fake, after: func(n int32) {
					if n == 2 {
						cancel()
					}
				}}
				err = streamRunContainerWithStarted(ctx, conn, stream, cfg, opts, nil)
				if got := fake.stops(); len(got) != 1 || got[0] != "app" {
					t.Fatalf("StopContainer calls = %v, want [app] (the interrupt's stop)", got)
				}
				if notes.outcome != interruptedAppStopped {
					t.Fatalf("notes = %+v, want stopped", notes)
				}
			case "follow":
				// Poll 1 is the gate's baseline; the logs then end, so poll 2
				// is the follow seeing the exit and waiting for the gate;
				// poll 3 is the gate's first tick a second later: interrupt
				// then.
				listed := make(chan struct{})
				conn.ContainerService = &listHook{scriptedContainerClient: fake, after: func(n int32) {
					switch n {
					case 1:
						close(listed)
					case 3:
						cancel()
					}
				}}
				conn.TelemetryService = &logsEndAfter{after: listed}
				err = followExistingContainer(ctx, conn, cfg, opts, appBaseline{})
				if got := fake.stops(); len(got) != 0 {
					t.Fatalf("StopContainer calls = %v, want none", got)
				}
				if notes.outcome != interruptedAppLeftRunning {
					t.Fatalf("notes = %+v, want left running", notes)
				}
			}
			if !errors.Is(err, ErrUserCancelled) {
				t.Fatalf("err = %v, want ErrUserCancelled", err)
			}
		})
	}
}
