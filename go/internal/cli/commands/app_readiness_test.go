package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

// scriptedState returns a state func that walks snapshots (repeating the last)
// and an optional error script aligned with it.
func scriptedState(snapshots []*agentpb.AppContainer, errs ...error) func(context.Context) (*agentpb.AppContainer, error) {
	var mu sync.Mutex
	i := 0
	return func(context.Context) (*agentpb.AppContainer, error) {
		mu.Lock()
		defer mu.Unlock()
		n := min(i, len(snapshots)-1)
		var err error
		if i < len(errs) {
			err = errs[i]
		}
		i++
		return snapshots[n], err
	}
}

func bufferedTicks(n int) chan time.Time {
	c := make(chan time.Time, n)
	for range n {
		c <- time.Time{}
	}
	return c
}

func firedDeadline() chan time.Time {
	c := make(chan time.Time, 1)
	c <- time.Time{}
	return c
}

func runningSnapshot(failures uint32) *agentpb.AppContainer {
	return appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", failures)
}

func TestAwaitAppReady(t *testing.T) {
	probeOnCall := func(n int) func(context.Context) bool {
		calls := 0
		return func(context.Context) bool { calls++; return calls >= n }
	}
	for _, tc := range []struct {
		name          string
		checks        waitReadyChecks
		status        string
		readiness     string
		class         string
		exitCode      int32
		wantExitZero  bool // exitCode 0 means "not checked"; this requires a recorded 0
		wantInMessage string
	}{
		{
			name:      "probe passes on the second poll",
			checks:    waitReadyChecks{tick: bufferedTicks(3), probe: probeOnCall(2), state: scriptedState([]*agentpb.AppContainer{runningSnapshot(0)})},
			status:    waitReadyStatusReady,
			readiness: readinessPassed,
		},
		{
			name:      "no probe: running when the window ends",
			checks:    waitReadyChecks{tick: make(chan time.Time), deadline: firedDeadline(), state: scriptedState([]*agentpb.AppContainer{runningSnapshot(0)})},
			status:    waitReadyStatusRunning,
			readiness: readinessNotChecked,
		},
		{
			name:          "no probe: exits inside the window",
			checks:        waitReadyChecks{tick: bufferedTicks(1), state: scriptedState([]*agentpb.AppContainer{runningSnapshot(0), appSnapshot("app", agentpb.AppRunningState_STOPPED, 3, "crashed", 0)})},
			status:        waitReadyStatusCrashed,
			readiness:     readinessNotChecked,
			class:         "app_crashed",
			exitCode:      3,
			wantInMessage: "exit code 3",
		},
		{
			// --wait-ready promised a running app, so a clean exit fails too.
			name:          "no probe: exits cleanly inside the window",
			checks:        waitReadyChecks{tick: bufferedTicks(1), state: scriptedState([]*agentpb.AppContainer{runningSnapshot(0), appSnapshot("app", agentpb.AppRunningState_STOPPED, 0, "exited", 0)})},
			status:        waitReadyStatusCrashed,
			readiness:     readinessNotChecked,
			class:         "app_crashed",
			wantExitZero:  true,
			wantInMessage: "exited before becoming ready (exit code 0)",
		},
		{
			name:          "no probe: restarted by the restart policy inside the window",
			checks:        waitReadyChecks{tick: bufferedTicks(1), state: scriptedState([]*agentpb.AppContainer{runningSnapshot(2), runningSnapshot(3)})},
			status:        waitReadyStatusCrashed,
			readiness:     readinessNotChecked,
			class:         "app_crashed",
			wantInMessage: "restarted 1 time(s)",
		},
		{
			name:          "probe never passes",
			checks:        waitReadyChecks{appID: "app", limit: 30 * time.Second, tick: make(chan time.Time), deadline: firedDeadline(), probe: func(context.Context) bool { return false }, state: scriptedState([]*agentpb.AppContainer{runningSnapshot(0)})},
			status:        waitReadyStatusNotReady,
			readiness:     readinessFailed,
			class:         "readiness_timeout",
			wantInMessage: "did not pass its readiness probe within 30s",
		},
		{
			name:      "transient state errors are tolerated",
			checks:    waitReadyChecks{tick: bufferedTicks(3), probe: probeOnCall(2), state: scriptedState([]*agentpb.AppContainer{runningSnapshot(0)}, nil, errors.New("agent busy"))},
			status:    waitReadyStatusReady,
			readiness: readinessPassed,
		},
		{
			name:      "probe passes but the app is gone by the confirmation poll",
			checks:    waitReadyChecks{tick: bufferedTicks(1), probe: probeOnCall(1), state: scriptedState([]*agentpb.AppContainer{runningSnapshot(0), runningSnapshot(0), appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 1, "crashed", 1)})},
			status:    waitReadyStatusCrashed,
			readiness: readinessFailed,
			class:     "app_crashed",
			exitCode:  1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.checks.appID == "" {
				tc.checks.appID = "app"
			}
			out := awaitAppReady(context.Background(), tc.checks)
			if out.Status != tc.status || out.Readiness != tc.readiness {
				t.Fatalf("outcome = %+v, want status %q readiness %q", out, tc.status, tc.readiness)
			}
			if got := ErrorClass(out.err); got != tc.class {
				t.Fatalf("class = %q (err %v), want %q", got, out.err, tc.class)
			}
			if tc.exitCode != 0 && (out.ExitCode == nil || *out.ExitCode != tc.exitCode) {
				t.Fatalf("exit code = %v, want %d", out.ExitCode, tc.exitCode)
			}
			if tc.wantExitZero && (out.ExitCode == nil || *out.ExitCode != 0) {
				t.Fatalf("exit code = %v, want a recorded 0", out.ExitCode)
			}
			if !strings.Contains(out.Message, tc.wantInMessage) {
				t.Fatalf("message %q does not contain %q", out.Message, tc.wantInMessage)
			}
		})
	}
}

func TestAwaitAppReadyInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := awaitAppReady(ctx, waitReadyChecks{appID: "app", tick: make(chan time.Time), state: scriptedState([]*agentpb.AppContainer{runningSnapshot(0)})})
	if out.Status != "" || !errors.Is(out.err, context.Canceled) {
		t.Fatalf("outcome = %+v, want an empty status and context.Canceled", out)
	}
}

func TestWaitForAppReadyProbesTheLANAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := testPort(t, ln)
	conn := &grpcclient.AgentConnection{
		Host:             "127.0.0.1",
		ContainerService: &scriptedContainerClient{snapshots: []*agentpb.AppContainer{runningSnapshot(0)}},
		AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
			NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"127.0.0.1"}}},
		}},
	}
	cfg := &appconfig.AppConfig{AppID: "app", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: port}}}
	out := waitForAppReady(context.Background(), conn, cfg, runOptions{})
	if out.Status != waitReadyStatusReady || out.err != nil {
		t.Fatalf("outcome = %+v, want ready", out)
	}
	if want := "http://127.0.0.1:" + strconv.Itoa(port); out.URL != want || out.App != "app" || out.Device != "127.0.0.1" {
		t.Fatalf("outcome = %+v, want url %q", out, want)
	}
}

// Through the cloud tunnel the agent-reported LAN address is often not
// reachable from this machine; --wait-ready then falls back to the stability
// window instead of probing (or printing) that address.
func TestWaitForAppReadyCloudUnreachableLANUsesTheStabilityWindow(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dialed := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			dialed <- struct{}{}
			c.Close()
		}
	}()
	checked := stubLANAddressReachable(t, false)
	port := testPort(t, ln)
	conn := &grpcclient.AgentConnection{
		Host:             "cctv",
		Reconnect:        neverReconnect,
		ContainerService: &scriptedContainerClient{snapshots: []*agentpb.AppContainer{runningSnapshot(0)}},
		AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
			NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"127.0.0.1"}}},
		}},
	}
	cfg := &appconfig.AppConfig{AppID: "app", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: port}}}
	out := waitForAppReady(context.Background(), conn, cfg, runOptions{readinessTimeout: time.Second})
	if out.Status != waitReadyStatusRunning || out.Readiness != readinessNotChecked || out.URL != "" {
		t.Fatalf("outcome = %+v, want running / not_checked without a URL", out)
	}
	select {
	case <-dialed:
		t.Fatal("dialed the LAN address through the cloud tunnel")
	default:
	}
	if len(*checked) != 1 || (*checked)[0] != "127.0.0.1" {
		t.Fatalf("lanAddressReachable asked about %v, want exactly [127.0.0.1]", *checked)
	}
}

// When the cloud device's agent answers on its LAN address from this machine
// (the developer is on its network), --wait-ready probes that address as on a
// LAN connection and reports its URL (WDY-2440).
func TestWaitForAppReadyCloudReachableLANProbes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := testPort(t, ln)
	checked := stubLANAddressReachable(t, true) // no agent listens on 127.0.0.1 here
	conn := &grpcclient.AgentConnection{
		Host:             "cctv",
		Reconnect:        neverReconnect,
		ContainerService: &scriptedContainerClient{snapshots: []*agentpb.AppContainer{runningSnapshot(0)}},
		AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
			NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"127.0.0.1"}}},
		}},
	}
	cfg := &appconfig.AppConfig{AppID: "app", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: port}}}
	out := waitForAppReady(context.Background(), conn, cfg, runOptions{readinessTimeout: 5 * time.Second})
	if out.Status != waitReadyStatusReady || out.Readiness != readinessPassed || out.err != nil {
		t.Fatalf("outcome = %+v, want ready / passed", out)
	}
	if want := "http://127.0.0.1:" + strconv.Itoa(port); out.URL != want {
		t.Fatalf("url = %q, want %q", out.URL, want)
	}
	if len(*checked) != 1 || (*checked)[0] != "127.0.0.1" {
		t.Fatalf("lanAddressReachable asked about %v, want exactly [127.0.0.1]", *checked)
	}
}

// On the device (WENDY_AGENT_SOCKET), the connection's Host is "unix:<sock>":
// no address to probe the app at, and no device name to report. --wait-ready
// uses the stability window instead of probing "[unix:…]:<port>" into a false
// readiness_timeout, and the JSON object leaves "device" out.
func TestWaitReadyAgentSocketUsesTheStabilityWindow(t *testing.T) {
	previous := jsonOutput
	t.Cleanup(func() { jsonOutput = previous })
	jsonOutput = true
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dialed := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			dialed <- struct{}{}
			c.Close()
		}
	}()
	conn := &grpcclient.AgentConnection{
		Host:             "unix:/run/wendy/agent.sock",
		ContainerService: &scriptedContainerClient{snapshots: []*agentpb.AppContainer{runningSnapshot(0)}},
		// Were the reported IP used as the probe host, the listener would see it.
		AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
			NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"127.0.0.1"}}},
		}},
	}
	cfg := &appconfig.AppConfig{AppID: "app", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: testPort(t, ln)}}}
	var runErr error
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			runErr = waitReadyAfterDetachedStart(context.Background(), conn, cfg, runOptions{detach: true, waitReady: true, readinessTimeout: time.Second})
		})
	})
	if runErr != nil {
		t.Fatalf("err = %v, want a healthy app to pass", runErr)
	}
	got := decodeOneJSONObject(t, stdout)
	if got["status"] != waitReadyStatusRunning || got["readiness"] != readinessNotChecked || got["app"] != "app" {
		t.Fatalf("object = %v, want running / not_checked", got)
	}
	for _, key := range []string{"device", "url"} {
		if _, ok := got[key]; ok {
			t.Errorf("object carries %q: %v", key, got)
		}
	}
	if !strings.Contains(stderr, "WENDY_AGENT_SOCKET") {
		t.Errorf("stderr %q does not say why readiness is not checked", stderr)
	}
	select {
	case <-dialed:
		t.Fatal("probed the app over the network from the agent socket connection")
	default:
	}
}

// decodeOneJSONObject decodes stdout as exactly one JSON object followed by
// EOF, failing the test otherwise.
func decodeOneJSONObject(t *testing.T, stdout string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var got map[string]any
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("stdout %q is not JSON: %v", stdout, err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		t.Fatalf("stdout holds more than one JSON value: %q", stdout)
	}
	return got
}

func TestReportWaitReadyOutcomePrintsExactlyOneJSONObject(t *testing.T) {
	previous := jsonOutput
	t.Cleanup(func() { jsonOutput = previous })
	jsonOutput = true
	code := int32(3)
	crash := waitReadyOutcome{Status: waitReadyStatusCrashed, App: "wr-crash", Device: "dev.local", Readiness: readinessNotChecked, ExitCode: &code, TerminationReason: "crashed", Message: "app wr-crash stopped unexpectedly", err: errors.New("x")}
	cfg := &appconfig.AppConfig{AppID: "wr-crash"}
	stdout := captureStdout(t, func() { reportWaitReadyOutcome(context.Background(), cfg, crash, true) })
	got := decodeOneJSONObject(t, stdout)
	for key, want := range map[string]any{"status": "crashed", "app": "wr-crash", "device": "dev.local", "readiness": "not_checked", "exit_code": float64(3), "termination_reason": "crashed"} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
	if _, ok := got["url"]; ok {
		t.Error("a failed outcome carries a url")
	}
	// Attached runs stream app output on stdout, so they never add the object.
	if out := captureStdout(t, func() { reportWaitReadyOutcome(context.Background(), cfg, crash, false) }); out != "" {
		t.Fatalf("attached report wrote %q to stdout", out)
	}
}

// A clean exit inside the wait is a failure whose JSON object still carries
// the recorded exit code 0 (a nil *int32 would drop it via omitempty).
func TestReportWaitReadyOutcomeCleanExitCarriesExitCodeZero(t *testing.T) {
	previous := jsonOutput
	t.Cleanup(func() { jsonOutput = previous })
	jsonOutput = true
	out := awaitAppReady(context.Background(), waitReadyChecks{appID: "app", tick: bufferedTicks(1), state: scriptedState([]*agentpb.AppContainer{runningSnapshot(0), appSnapshot("app", agentpb.AppRunningState_STOPPED, 0, "exited", 0)})})
	out.App = "app"
	stdout := captureStdout(t, func() { reportWaitReadyOutcome(context.Background(), &appconfig.AppConfig{AppID: "app"}, out, true) })
	if !strings.Contains(stdout, `"exit_code":0`) {
		t.Fatalf("stdout %q does not carry \"exit_code\":0", stdout)
	}
	got := decodeOneJSONObject(t, stdout)
	if got["status"] != "crashed" || got["termination_reason"] != "exited" || !strings.Contains(fmt.Sprint(got["message"]), "exited before becoming ready (exit code 0)") {
		t.Fatalf("object = %v, want a crashed clean exit", got)
	}
}

func TestWaitReadyAfterDetachedStartInterruptedPrintsNothing(t *testing.T) {
	previous := jsonOutput
	t.Cleanup(func() { jsonOutput = previous })
	jsonOutput = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ctx, notes := withInterruptNotes(ctx)
	fake := &scriptedContainerClient{snapshots: []*agentpb.AppContainer{runningSnapshot(0)}}
	conn := &grpcclient.AgentConnection{Host: "dev", ContainerService: fake}
	var err error
	stdout := captureStdout(t, func() { err = waitReadyAfterDetachedStart(ctx, conn, &appconfig.AppConfig{AppID: "app"}, runOptions{}) })
	if !errors.Is(err, context.Canceled) || stdout != "" {
		t.Fatalf("err = %v stdout = %q, want context.Canceled and no JSON", err, stdout)
	}
	if notes.outcome != interruptedAppLeftRunning || notes.app != "app" {
		t.Fatalf("notes = %+v, want app left running", notes)
	}
	if got := fake.stops(); len(got) != 0 {
		t.Fatalf("StopContainer calls = %v, want none: a detached app stays running", got)
	}
}

// fastPathScriptedClient drives tryDeployFastPath's present-but-stopped
// branch: the device holds every layer (QueryLayers), and ListContainers and
// StartContainer come from the scripted client.
type fastPathScriptedClient struct {
	*scriptedContainerClient
}

func (c fastPathScriptedClient) QueryLayers(_ context.Context, in *agentpb.QueryLayersRequest, _ ...grpc.CallOption) (*agentpb.QueryLayersResponse, error) {
	resp := &agentpb.QueryLayersResponse{}
	for _, id := range in.GetDiffIds() {
		resp.Present = append(resp.Present, &agentpb.PresentLayer{DiffId: id, Size: 1})
	}
	return resp, nil
}

// stoppedThenStarted scripts the fast path's stopped branch: its lookup sees
// the app stopped, then the wait sees states.
func stoppedThenStarted(states ...*agentpb.AppContainer) *scriptedContainerClient {
	snapshots := append([]*agentpb.AppContainer{appSnapshot("app", agentpb.AppRunningState_STOPPED, 0, "", 0)}, states...)
	return &scriptedContainerClient{snapshots: snapshots, stream: &deploymentAckStream{remaining: 1, err: io.EOF}}
}

func TestDetachedPathsWaitForReadinessWhenAsked(t *testing.T) {
	crashed := []*agentpb.AppContainer{runningSnapshot(0), appSnapshot("app", agentpb.AppRunningState_STOPPED, 3, "crashed", 0)}
	started := func() *deploymentAckStream { return &deploymentAckStream{remaining: 1, err: io.EOF} }
	opts := runOptions{detach: true, waitReady: true, readinessTimeout: time.Second}
	cfg := &appconfig.AppConfig{AppID: "app"}

	t.Run("chunk-diff", func(t *testing.T) {
		conn := &grpcclient.AgentConnection{Host: "dev", ContainerService: &scriptedContainerClient{snapshots: crashed}}
		if err := streamRunContainerWithStarted(context.Background(), conn, started(), cfg, opts, nil); ErrorClass(err) != "app_crashed" {
			t.Fatalf("err = %v, want app_crashed", err)
		}
	})
	t.Run("registry", func(t *testing.T) {
		conn := &grpcclient.AgentConnection{Host: "dev", ContainerService: &scriptedContainerClient{snapshots: crashed, stream: started()}}
		if err := startExistingContainer(context.Background(), conn, cfg, opts); ErrorClass(err) != "app_crashed" {
			t.Fatalf("err = %v, want app_crashed", err)
		}
	})
	t.Run("no-change fast path", func(t *testing.T) {
		isolateFingerprintCache(t)
		saveDeployFingerprint("app", "device", deployFingerprint{InputHash: "inputs", LayerDiffIDs: []string{"layer"}})
		fake := &fastPathContainerClient{appName: "app", state: agentpb.AppRunningState_RUNNING, presentLayers: map[string]bool{"layer": true}}
		conn := &grpcclient.AgentConnection{Host: "dev", ContainerService: fake}
		done, err := tryDeployFastPath(context.Background(), conn, cfg, "device", "inputs", opts)
		if !done || err != nil {
			t.Fatalf("done=%v err=%v, want a running app to pass the 1s window", done, err)
		}
	})
	t.Run("no-change fast path starting a stopped app", func(t *testing.T) {
		isolateFingerprintCache(t)
		saveDeployFingerprint("app", "device", deployFingerprint{InputHash: "inputs", LayerDiffIDs: []string{"layer"}})
		conn := &grpcclient.AgentConnection{Host: "dev", ContainerService: fastPathScriptedClient{stoppedThenStarted(crashed...)}}
		done, err := tryDeployFastPath(context.Background(), conn, cfg, "device", "inputs", opts)
		if !done || ErrorClass(err) != "app_crashed" {
			t.Fatalf("done=%v err=%v, want done with app_crashed", done, err)
		}
	})
	t.Run("without --wait-ready nothing waits", func(t *testing.T) {
		conn := &grpcclient.AgentConnection{Host: "dev", ContainerService: &scriptedContainerClient{snapshots: crashed}}
		if err := streamRunContainerWithStarted(context.Background(), conn, started(), cfg, runOptions{detach: true}, nil); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
	})
}

// outputThenHoldStream is a start stream for an app that keeps printing: it
// acknowledges Started, serves n output messages, then stays open until
// release is closed and ends with io.EOF. drained is closed once a reader
// came back after the n-th message (all n consumed), ended once the stream
// has ended.
type outputThenHoldStream struct {
	grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse]
	n                       int
	release, drained, ended chan struct{}

	mu    sync.Mutex
	recvs int
}

func newOutputThenHoldStream(n int) *outputThenHoldStream {
	return &outputThenHoldStream{n: n, release: make(chan struct{}), drained: make(chan struct{}), ended: make(chan struct{})}
}

func (s *outputThenHoldStream) Recv() (*agentpb.RunContainerLayersResponse, error) {
	s.mu.Lock()
	s.recvs++
	call := s.recvs
	s.mu.Unlock()
	switch {
	case call == 1:
		return &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_Started_{Started: &agentpb.RunContainerLayersResponse_Started{}}}, nil
	case call <= s.n+1:
		return &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: []byte("still busy\n")}}}, nil
	case call == s.n+2:
		close(s.drained)
		<-s.release
		close(s.ended)
	}
	return nil, io.EOF
}

// calls is how many times Recv was called.
func (s *outputThenHoldStream) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recvs
}

// drainGatedClient holds every ListContainers call made after its stream
// acknowledged Started until the stream is drained, so --wait-ready's polls,
// and with them its outcome, can only come after all the output was
// consumed. Once giveUp is closed it holds no call any more: a run that does
// not drain then fails its test instead of hanging it.
type drainGatedClient struct {
	fastPathScriptedClient
	stream   *outputThenHoldStream
	giveUp   <-chan struct{}
	startCtx context.Context // the StartContainer RPC's, if one was made
}

func (c *drainGatedClient) StartContainer(ctx context.Context, _ *agentpb.StartContainerRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse], error) {
	c.startCtx = ctx
	return c.stream, nil
}

func (c *drainGatedClient) ListContainers(ctx context.Context, req *agentpb.ListContainersRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.ListContainersResponse], error) {
	if c.stream.calls() > 0 {
		select {
		case <-c.stream.drained:
		case <-c.giveUp:
		}
	}
	return c.fastPathScriptedClient.ListContainers(ctx, req, opts...)
}

// detachedStartPaths runs a detached start of cfg's app with opts on each
// single-container path whose start stream a --wait-ready wait outlives:
// chunk-diff (RunContainer), registry (StartContainer), and the no-change
// fast path starting a stopped app (StartContainer). lookups is what the
// device reports once the app started.
func detachedStartPaths(cfg *appconfig.AppConfig, opts runOptions, lookups ...*agentpb.AppContainer) []struct {
	name     string
	snapshot []*agentpb.AppContainer
	run      func(t *testing.T, conn *grpcclient.AgentConnection, stream *outputThenHoldStream) error
} {
	stopped := appSnapshot(cfg.AppID, agentpb.AppRunningState_STOPPED, 0, "", 0)
	return []struct {
		name     string
		snapshot []*agentpb.AppContainer
		run      func(t *testing.T, conn *grpcclient.AgentConnection, stream *outputThenHoldStream) error
	}{
		{"chunk-diff", lookups, func(_ *testing.T, conn *grpcclient.AgentConnection, stream *outputThenHoldStream) error {
			return streamRunContainerWithStarted(context.Background(), conn, stream, cfg, opts, nil)
		}},
		{"registry", lookups, func(_ *testing.T, conn *grpcclient.AgentConnection, _ *outputThenHoldStream) error {
			return startExistingContainer(context.Background(), conn, cfg, opts)
		}},
		{"no-change fast path starting a stopped app", append([]*agentpb.AppContainer{stopped}, lookups...), func(t *testing.T, conn *grpcclient.AgentConnection, _ *outputThenHoldStream) error {
			isolateFingerprintCache(t)
			saveDeployFingerprint(cfg.AppID, "device", deployFingerprint{InputHash: "inputs", LayerDiffIDs: []string{"layer"}})
			done, err := tryDeployFastPath(context.Background(), conn, cfg, "device", "inputs", opts)
			if !done {
				t.Fatalf("fast path fell back to a full deploy (err %v)", err)
			}
			return err
		}},
	}
}

// While `--detach --wait-ready` waits, the start stream stays open and the
// app keeps printing into it. The run reads and discards that output, so an
// agent that applies backpressure to an unread stream cannot stall a chatty
// app into a false readiness_timeout. It does not cancel the stream, and the
// drain stops once the stream ends.
func TestDetachedWaitReadyDrainsTheStartStream(t *testing.T) {
	const lines = 50
	cfg := &appconfig.AppConfig{AppID: "app"}
	opts := runOptions{detach: true, waitReady: true, readinessTimeout: time.Second}
	for _, path := range detachedStartPaths(cfg, opts, runningSnapshot(0)) {
		t.Run(path.name, func(t *testing.T) {
			giveUp, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream := newOutputThenHoldStream(lines)
			scripted := &scriptedContainerClient{snapshots: path.snapshot, stream: stream}
			client := &drainGatedClient{fastPathScriptedClient: fastPathScriptedClient{scripted}, stream: stream, giveUp: giveUp.Done()}
			conn := &grpcclient.AgentConnection{Host: "dev", ContainerService: client}

			if err := path.run(t, conn, stream); err != nil {
				t.Fatalf("err = %v, want the app reported running", err)
			}
			select {
			case <-stream.drained:
			default:
				t.Fatalf("the wait finished after %d Recv calls, want %d: the start stream's %d output messages were not all read", stream.calls(), lines+2, lines)
			}
			if client.startCtx != nil && client.startCtx.Err() != nil {
				t.Fatalf("the StartContainer RPC's context was cancelled (%v); the stream must stay the agent's to end", client.startCtx.Err())
			}
			close(stream.release)
			select {
			case <-stream.ended: // the drain saw the stream end and stopped
			case <-time.After(5 * time.Second):
				t.Fatal("nothing read the stream's end: the drain did not keep reading until it ended")
			}
		})
	}
}

// Without --wait-ready a detached run still returns at Started and leaves
// the start stream alone.
func TestDetachedRunWithoutWaitReadyDoesNotDrain(t *testing.T) {
	previous := drainDetachedStartOutput
	t.Cleanup(func() { drainDetachedStartOutput = previous })
	drains := 0
	drainDetachedStartOutput = func(containerOutputStream) { drains++ }
	cfg := &appconfig.AppConfig{AppID: "app"}
	for _, path := range detachedStartPaths(cfg, runOptions{detach: true}, runningSnapshot(0)) {
		t.Run(path.name, func(t *testing.T) {
			drains = 0
			stream := newOutputThenHoldStream(3)
			scripted := &scriptedContainerClient{snapshots: path.snapshot, stream: stream}
			conn := &grpcclient.AgentConnection{Host: "dev", ContainerService: fastPathScriptedClient{scripted}}
			if err := path.run(t, conn, stream); err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if drains != 0 || stream.calls() > 1 {
				t.Fatalf("drains = %d, Recv calls = %d: a detached run without --wait-ready must not read past Started", drains, stream.calls())
			}
		})
	}
}

// A `--detach --wait-ready` run in JSON mode prints exactly one JSON object on
// stdout on every detached single-container path, failures included, and the
// stdout guard RunE applies keeps build progress off stdout.
func TestDetachedWaitReadyJSONStdoutIsExactlyOneObject(t *testing.T) {
	previousJSON, previousOut := jsonOutput, buildProgressOut
	t.Cleanup(func() { jsonOutput, buildProgressOut = previousJSON, previousOut })
	jsonOutput = true
	opts := runOptions{detach: true, waitReady: true, readinessTimeout: time.Second}
	cfg := &appconfig.AppConfig{AppID: "app"}
	healthy := []*agentpb.AppContainer{runningSnapshot(0)}
	crashed := []*agentpb.AppContainer{runningSnapshot(0), appSnapshot("app", agentpb.AppRunningState_STOPPED, 3, "crashed", 0)}
	started := func() *deploymentAckStream { return &deploymentAckStream{remaining: 1, err: io.EOF} }
	chunkDiff := func(snapshots []*agentpb.AppContainer) func(*testing.T) error {
		return func(*testing.T) error {
			conn := &grpcclient.AgentConnection{Host: "dev", ContainerService: &scriptedContainerClient{snapshots: snapshots}}
			return streamRunContainerWithStarted(context.Background(), conn, started(), cfg, opts, nil)
		}
	}
	registry := func(snapshots []*agentpb.AppContainer) func(*testing.T) error {
		return func(*testing.T) error {
			conn := &grpcclient.AgentConnection{Host: "dev", ContainerService: &scriptedContainerClient{snapshots: snapshots, stream: started()}}
			return startExistingContainer(context.Background(), conn, cfg, opts)
		}
	}
	fastPath := func(client agentpb.WendyContainerServiceClient) func(*testing.T) error {
		return func(t *testing.T) error {
			isolateFingerprintCache(t)
			saveDeployFingerprint("app", "device", deployFingerprint{InputHash: "inputs", LayerDiffIDs: []string{"layer"}})
			done, err := tryDeployFastPath(context.Background(), &grpcclient.AgentConnection{Host: "dev", ContainerService: client}, cfg, "device", "inputs", opts)
			if !done {
				// Errorf, not Fatalf: this runs inside captureStdout.
				t.Errorf("fast path fell back to a full deploy (err %v)", err)
			}
			return err
		}
	}
	for _, tc := range []struct {
		name   string
		run    func(*testing.T) error
		status string
	}{
		{"chunk-diff healthy", chunkDiff(healthy), waitReadyStatusRunning},
		{"chunk-diff crash", chunkDiff(crashed), waitReadyStatusCrashed},
		{"registry healthy", registry(healthy), waitReadyStatusRunning},
		{"registry crash", registry(crashed), waitReadyStatusCrashed},
		{"no-change fast path running", fastPath(&fastPathContainerClient{appName: "app", state: agentpb.AppRunningState_RUNNING, presentLayers: map[string]bool{"layer": true}}), waitReadyStatusRunning},
		{"no-change fast path started", fastPath(fastPathScriptedClient{stoppedThenStarted(healthy...)}), waitReadyStatusRunning},
		{"no-change fast path started, then crash", fastPath(fastPathScriptedClient{stoppedThenStarted(crashed...)}), waitReadyStatusCrashed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			stdout := captureStdout(t, func() {
				// Point build progress at the captured stdout, as production
				// does, so only the guard keeps it off.
				defer setBuildProgressOut(os.Stdout)()
				defer waitReadyJSONStdoutGuard(opts)()
				fmt.Fprintln(buildProgressOut, "#1 [internal] load build definition")
				err = tc.run(t)
			})
			got := decodeOneJSONObject(t, stdout)
			if got["status"] != tc.status || got["app"] != "app" || got["device"] != "dev" {
				t.Fatalf("object = %v, want status %q for app on dev", got, tc.status)
			}
			if wantErr := tc.status != waitReadyStatusRunning; (err != nil) != wantErr {
				t.Fatalf("err = %v, want an error: %v", err, wantErr)
			}
		})
	}
}
