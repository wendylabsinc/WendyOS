package commands

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// fakeAgentVersionClient stubs the single AgentService RPC that
// announceReachableURL issues, so runPostStartIfReady tests can control which
// device IPs the agent reports (or fail the query outright).
type fakeAgentVersionClient struct {
	agentpb.WendyAgentServiceClient // embedded nil — satisfies interface

	resp *agentpb.GetAgentVersionResponse
	err  error

	// calls counts GetAgentVersion invocations, so tests can assert the agent
	// was never queried at all (e.g. a hook-less/readiness-less app has
	// nothing to announce and must short-circuit before reaching the agent).
	calls int
}

func (f *fakeAgentVersionClient) GetAgentVersion(_ context.Context, _ *agentpb.GetAgentVersionRequest, _ ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	f.calls++
	return f.resp, f.err
}

func TestAgentVersionForRunReusesConnectionProbe(t *testing.T) {
	client := &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{Version: "unexpected-rpc"}}
	conn := &grpcclient.AgentConnection{AgentService: client}
	want := &agentpb.GetAgentVersionResponse{Version: "connect-probe", Os: "linux"}
	conn.CacheAgentVersion(want)

	got, err := agentVersionForRun(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("agentVersionForRun returned %p, want cached response %p", got, want)
	}
	if client.calls != 0 {
		t.Fatalf("GetAgentVersion calls = %d, want 0", client.calls)
	}
}

func TestAgentVersionForRunCachesFirstRPC(t *testing.T) {
	want := &agentpb.GetAgentVersionResponse{Version: "queried", Os: "linux"}
	client := &fakeAgentVersionClient{resp: want}
	conn := &grpcclient.AgentConnection{AgentService: client}

	for range 2 {
		got, err := agentVersionForRun(context.Background(), conn)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("agentVersionForRun returned %p, want %p", got, want)
		}
	}
	if client.calls != 1 {
		t.Fatalf("GetAgentVersion calls = %d, want 1", client.calls)
	}
}

func TestAgentVersionForRunHonorsCanceledContextBeforeCache(t *testing.T) {
	client := &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{Version: "unexpected-rpc"}}
	conn := &grpcclient.AgentConnection{AgentService: client}
	conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Version: "cached"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := agentVersionForRun(ctx, conn); !errors.Is(err, context.Canceled) {
		t.Fatalf("agentVersionForRun error = %v, want context.Canceled", err)
	}
	if client.calls != 0 {
		t.Fatalf("GetAgentVersion calls = %d, want 0", client.calls)
	}
}

// neverReconnect is a non-nil Reconnect stub used to mark an AgentConnection
// as a cloud connection (conn.Reconnect != nil is the cloud detection
// signal — see resolveHookHost). It is never actually invoked by these
// tests, which don't exercise agent-restart reconnection.
func neverReconnect(context.Context) (*grpcclient.AgentConnection, error) {
	return nil, errors.New("reconnect not implemented in test")
}

func testPort(t *testing.T, ln net.Listener) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi(%q): %v", portStr, err)
	}
	return port
}

func TestWaitForReadiness_NilConfig(t *testing.T) {
	err := waitForReadiness(context.Background(), nil, "localhost")
	if err != nil {
		t.Fatalf("expected nil error for nil config, got %v", err)
	}
}

func TestWaitForReadiness_NilTCPSocket(t *testing.T) {
	cfg := &appconfig.ReadinessConfig{}
	err := waitForReadiness(context.Background(), cfg, "localhost")
	if err != nil {
		t.Fatalf("expected nil error for nil tcpSocket, got %v", err)
	}
}

func TestWaitForReadiness_PortAlreadyListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()

	port := testPort(t, ln)

	cfg := &appconfig.ReadinessConfig{
		TCPSocket:      &appconfig.TCPSocketProbe{Port: port},
		TimeoutSeconds: 5,
	}

	start := time.Now()
	err = waitForReadiness(context.Background(), cfg, "127.0.0.1")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v, expected near-instant for already listening port", elapsed)
	}
}

func TestWaitForReadiness_PortBecomesAvailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	addr := ln.Addr().String()
	port := testPort(t, ln)
	ln.Close()

	cfg := &appconfig.ReadinessConfig{
		TCPSocket:      &appconfig.TCPSocketProbe{Port: port},
		TimeoutSeconds: 10,
	}

	go func() {
		time.Sleep(1 * time.Second)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		defer l.Close()
		<-time.After(10 * time.Second)
	}()

	start := time.Now()
	err = waitForReadiness(context.Background(), cfg, "127.0.0.1")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if elapsed < 500*time.Millisecond || elapsed > 5*time.Second {
		t.Errorf("took %v, expected ~1-2s", elapsed)
	}
}

func TestWaitForReadiness_Timeout(t *testing.T) {
	// Grab a free port from the OS, then release it immediately so nothing listens on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	port := testPort(t, ln)
	ln.Close()

	cfg := &appconfig.ReadinessConfig{
		TCPSocket:      &appconfig.TCPSocketProbe{Port: port},
		TimeoutSeconds: 2,
	}

	start := time.Now()
	err = waitForReadiness(context.Background(), cfg, "127.0.0.1")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed < 1*time.Second || elapsed > 5*time.Second {
		t.Errorf("took %v, expected ~2s", elapsed)
	}
}

func TestWaitForReadiness_ContextCancelled(t *testing.T) {
	// Grab a free port from the OS, then release it immediately so nothing listens on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	port := testPort(t, ln)
	ln.Close()

	cfg := &appconfig.ReadinessConfig{
		TCPSocket:      &appconfig.TCPSocketProbe{Port: port},
		TimeoutSeconds: 30,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err = waitForReadiness(ctx, cfg, "127.0.0.1")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}
	// Should return context.Canceled, not a timeout error.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("took %v, expected ~500ms (context cancel)", elapsed)
	}
}

// TestRunPostStartIfReady_SkipsHookWhenProbeFails locks the fix for the bug
// where `wendy run` printed "App reachable at ..." and opened the postStart
// URL even after the readiness probe timed out and the container had exited.
func TestRunPostStartIfReady_SkipsHookWhenProbeFails(t *testing.T) {
	original := browserOpen
	t.Cleanup(func() { browserOpen = original })
	opened := false
	browserOpen = func(string) error {
		opened = true
		return nil
	}

	// Grab a free port, then release it so nothing listens and the probe fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	port := testPort(t, ln)
	ln.Close()

	appCfg := &appconfig.AppConfig{
		AppID:     "not-ready-app",
		Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: port}, TimeoutSeconds: 1},
		Hooks: &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{
				OpenURL: "http://${WENDY_HOSTNAME}:3001",
				CLI:     "echo should-not-run",
			},
		},
	}
	// ContainerService backs warnReadiness's exit-detail lookup. AgentService
	// must be non-nil: resolveHookHost now resolves the probe/hook host BEFORE
	// the readiness wait (so a cloud asset name gets swapped before dialing),
	// which means announceReachableURL runs regardless of whether the probe
	// later fails — a nil AgentService would panic on that call. The err here
	// keeps announceReachableURL from printing anything (ip resolves to ""),
	// matching this test's focus on the failed-probe path, not the announcement.
	conn := &grpcclient.AgentConnection{
		Host:             "127.0.0.1",
		AgentService:     &fakeAgentVersionClient{err: errors.New("not needed for this test")},
		ContainerService: &fastPathContainerClient{appName: appCfg.AppID, state: agentpb.AppRunningState_STOPPED},
	}

	cmd := runPostStartIfReady(context.Background(), context.Background(), conn, appCfg, runOptions{})
	if cmd != nil {
		t.Errorf("runPostStartIfReady returned a hook cmd despite a failed probe")
	}
	if opened {
		t.Errorf("postStart openURL fired despite a failed readiness probe")
	}
}

// TestRunPostStartIfReady_IPv6HostSwappedForReportedIP verifies that when the
// CLI dialed the device at an IPv6 literal (often a rotating RFC 4941
// temporary address from mDNS), both the readiness probe and the postStart
// openURL target the device's best self-reported IP instead — the same
// address the "App reachable at" line prints. The readiness listener is
// bound at the reported IP (not the IPv6 literal): resolveHookHost now
// resolves the swap BEFORE waitForReadiness runs, so the probe itself must
// dial the swapped host too, not just the hook.
func TestRunPostStartIfReady_IPv6HostSwappedForReportedIP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()
	port := testPort(t, ln)

	original := browserOpen
	t.Cleanup(func() { browserOpen = original })
	var opened string
	browserOpen = func(url string) error {
		opened = url
		return nil
	}

	appCfg := &appconfig.AppConfig{
		AppID:     "v6-app",
		Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: port}, TimeoutSeconds: 5},
		Hooks: &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{OpenURL: "http://${WENDY_HOSTNAME}:3001"},
		},
	}
	conn := &grpcclient.AgentConnection{
		Host: "::1",
		AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
			NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"127.0.0.1"}}},
		}},
	}

	if cmd := runPostStartIfReady(context.Background(), context.Background(), conn, appCfg, runOptions{}); cmd != nil {
		t.Errorf("expected nil cmd for openURL-only hook, got %v", cmd)
	}
	if opened != "http://127.0.0.1:3001" {
		t.Errorf("openURL = %q, want the device-reported IPv4 URL", opened)
	}
}

// TestRunPostStartIfReady_IPv6FallbackIsBracketed verifies that when the
// device's IPs can't be queried, the hook falls back to the dialed IPv6
// literal — bracketed, so the URL parses (an unbracketed IPv6 literal reads
// the port as one more hextet).
func TestRunPostStartIfReady_IPv6FallbackIsBracketed(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer ln.Close()
	port := testPort(t, ln)

	original := browserOpen
	t.Cleanup(func() { browserOpen = original })
	var opened string
	browserOpen = func(url string) error {
		opened = url
		return nil
	}

	appCfg := &appconfig.AppConfig{
		AppID:     "v6-fallback-app",
		Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: port}, TimeoutSeconds: 5},
		Hooks: &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{OpenURL: "http://${WENDY_HOSTNAME}:3001"},
		},
	}
	conn := &grpcclient.AgentConnection{
		Host:         "::1",
		AgentService: &fakeAgentVersionClient{err: errors.New("agent unreachable")},
	}

	runPostStartIfReady(context.Background(), context.Background(), conn, appCfg, runOptions{})
	if opened != "http://[::1]:3001" {
		t.Errorf("openURL = %q, want bracketed IPv6 fallback URL", opened)
	}
}

// TestResolveHookHost is a pure table test of the host-resolution logic:
// LAN connections pass conn.Host through unchanged regardless of what the
// agent reports; an IPv6-literal Host swaps in the agent-reported IP; a cloud
// connection (conn.Reconnect != nil) swaps in the agent-reported IP only when
// the device's agent answers on that LAN address in the one quick
// reachability check; and a cloud connection with no reported IP, or whose
// LAN address does not answer, reports ok=false so the caller can skip
// host-side probes/hooks instead of dialing an address this machine cannot
// reach.
func TestResolveHookHost(t *testing.T) {
	appCfgWithHook := func() *appconfig.AppConfig {
		return &appconfig.AppConfig{
			AppID: "resolve-host-app",
			Hooks: &appconfig.HooksConfig{
				PostStart: &appconfig.HookCommand{OpenURL: "http://${WENDY_HOSTNAME}:80"},
			},
		}
	}

	cases := []struct {
		name string
		conn *grpcclient.AgentConnection
		// lanReachable is what the stubbed lanAddressReachable answers;
		// wantChecked is the LAN IP it must have been asked about ("" =
		// never asked: only cloud connections with a reported IP check).
		lanReachable bool
		wantChecked  string
		wantHost     string
		wantOK       bool
	}{
		{
			name: "LAN passthrough regardless of reported IP",
			conn: &grpcclient.AgentConnection{
				Host: "192.168.1.50",
				AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
					NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"10.0.0.5"}}},
				}},
			},
			wantHost: "192.168.1.50",
			wantOK:   true,
		},
		{
			name: "IPv6 literal swapped for reported IP",
			conn: &grpcclient.AgentConnection{
				Host: "::1",
				AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
					NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"192.168.1.77"}}},
				}},
			},
			wantHost: "192.168.1.77",
			wantOK:   true,
		},
		{
			// WDY-2440: the developer is on the device's LAN, so the reported
			// IP answers and replaces the unresolvable asset name.
			name: "cloud connection uses the reported IP when its LAN address answers",
			conn: &grpcclient.AgentConnection{
				Host:      "cctv",
				Reconnect: neverReconnect,
				AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
					NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"10.10.10.10"}}},
				}},
			},
			lanReachable: true,
			wantChecked:  "10.10.10.10",
			wantHost:     "10.10.10.10",
			wantOK:       true,
		},
		{
			// The reported IP is the device's LAN address, which a machine
			// using the cloud tunnel from elsewhere cannot reach.
			name: "cloud connection skips an unreachable LAN address",
			conn: &grpcclient.AgentConnection{
				Host:      "cctv",
				Reconnect: neverReconnect,
				AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
					NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"10.10.10.10"}}},
				}},
			},
			lanReachable: false,
			wantChecked:  "10.10.10.10",
			wantHost:     "",
			wantOK:       false,
		},
		{
			name: "cloud connection with no reported IP is not ok",
			conn: &grpcclient.AgentConnection{
				Host:         "cctv",
				Reconnect:    neverReconnect,
				AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}},
			},
			wantHost: "",
			wantOK:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checked := stubLANAddressReachable(t, tc.lanReachable)
			host, ok := resolveHookHost(context.Background(), tc.conn, appCfgWithHook())
			if host != tc.wantHost || ok != tc.wantOK {
				t.Errorf("resolveHookHost() = (%q, %v), want (%q, %v)", host, ok, tc.wantHost, tc.wantOK)
			}
			if tc.wantChecked == "" && len(*checked) != 0 {
				t.Errorf("lanAddressReachable asked about %v, want never", *checked)
			}
			if tc.wantChecked != "" && (len(*checked) != 1 || (*checked)[0] != tc.wantChecked) {
				t.Errorf("lanAddressReachable asked about %v, want exactly [%s]", *checked, tc.wantChecked)
			}
		})
	}
}

// stubLANAddressReachable makes the cloud LAN reachability check answer
// reachable (or not) without dialing, and records every LAN IP it was asked
// about.
func stubLANAddressReachable(t *testing.T, reachable bool) *[]string {
	t.Helper()
	original := lanAddressReachable
	t.Cleanup(func() { lanAddressReachable = original })
	var mu sync.Mutex
	var checked []string
	lanAddressReachable = func(_ context.Context, ip string) bool {
		mu.Lock()
		defer mu.Unlock()
		checked = append(checked, ip)
		return reachable
	}
	return &checked
}

// TestLANAddressReachable pins the cloud LAN reachability check: it dials the
// device agent's ports on the LAN address — the mTLS port a provisioned
// (cloud) agent serves and the plaintext one — and only a completed connect
// counts. A refused connection (nothing listens there) is not reachable, and
// neither is a dial that times out.
func TestLANAddressReachable(t *testing.T) {
	if want := []int{defaultAgentPort + agentMTLSPortOffset, defaultAgentPort}; fmt.Sprint(cloudLANAgentPorts) != fmt.Sprint(want) {
		t.Fatalf("cloudLANAgentPorts = %v, want the agent's mTLS and plaintext ports %v", cloudLANAgentPorts, want)
	}
	original := cloudLANAgentPorts
	t.Cleanup(func() { cloudLANAgentPorts = original })

	// Listeners on ephemeral ports stand in for the agent's ports.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := testPort(t, closed)
	closed.Close()

	cloudLANAgentPorts = []int{closedPort, testPort(t, ln)}
	if !lanAddressReachable(context.Background(), "127.0.0.1") {
		t.Error("an agent port that accepts connections did not count as reachable")
	}
	cloudLANAgentPorts = []int{closedPort}
	if lanAddressReachable(context.Background(), "127.0.0.1") {
		t.Error("a refused connection counted as reachable")
	}

	// TEST-NET-1 (RFC 5737) is never routed: the dial times out or fails
	// with no route. The context's deadline bounds the wait.
	cloudLANAgentPorts = original
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if lanAddressReachable(ctx, "192.0.2.1") {
		t.Error("an unroutable address counted as reachable")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v, want about the 300ms deadline", elapsed)
	}
}

// TestRunPostStartIfReady_CloudAssetNameSwappedForReportedIP verifies the
// core WDY-2440 fix, kept when the device's LAN address answers from this
// machine: when the CLI connected via the cloud tunnel, conn.Host is the cloud
// ASSET NAME (cloud_tunnel.go: agentConn.Host = asset.GetName()), which does
// not resolve from this machine. The postStart hook must target the
// agent-reported IP instead of dialing the dead name — after one quick check
// that the device's agent answers on the reported IP.
func TestRunPostStartIfReady_CloudAssetNameSwappedForReportedIP(t *testing.T) {
	original := browserOpen
	t.Cleanup(func() { browserOpen = original })
	var opened string
	browserOpen = func(url string) error {
		opened = url
		return nil
	}
	checked := stubLANAddressReachable(t, true)

	appCfg := &appconfig.AppConfig{
		AppID: "cloud-app",
		Hooks: &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{OpenURL: "http://${WENDY_HOSTNAME}:9999"},
		},
	}
	conn := &grpcclient.AgentConnection{
		Host:      "cctv",
		Reconnect: neverReconnect,
		AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
			NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"10.20.30.40"}}},
		}},
	}

	if cmd := runPostStartIfReady(context.Background(), context.Background(), conn, appCfg, runOptions{}); cmd != nil {
		t.Errorf("expected nil cmd for openURL-only hook, got %v", cmd)
	}
	if opened != "http://10.20.30.40:9999" {
		t.Errorf("openURL = %q, want the cloud-reported IP URL, not the unresolvable asset name %q", opened, conn.Host)
	}
	if len(*checked) != 1 || (*checked)[0] != "10.20.30.40" {
		t.Errorf("lanAddressReachable asked about %v, want exactly [10.20.30.40]", *checked)
	}
}

// TestRunPostStartIfReady_CloudUnreachableLANSkipsHostSideLifecycle: through
// the cloud tunnel, conn.Host is the asset name and the agent-reported IP is
// the device's LAN address. When that address does not answer from this
// machine, the run must not print it as "App reachable at" or open it; it says
// why the host-side hook is skipped instead.
func TestRunPostStartIfReady_CloudUnreachableLANSkipsHostSideLifecycle(t *testing.T) {
	original := browserOpen
	t.Cleanup(func() { browserOpen = original })
	opened := ""
	browserOpen = func(url string) error {
		opened = url
		return nil
	}
	stubLANAddressReachable(t, false)

	appCfg := &appconfig.AppConfig{
		AppID: "cloud-app",
		Hooks: &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{OpenURL: "http://${WENDY_HOSTNAME}:9999"},
		},
	}
	agentClient := &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
		NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"10.20.30.40"}}},
	}}
	conn := &grpcclient.AgentConnection{Host: "cctv", Reconnect: neverReconnect, AgentService: agentClient}

	out := captureStderr(t, func() {
		if cmd := runPostStartIfReady(context.Background(), context.Background(), conn, appCfg, runOptions{}); cmd != nil {
			t.Errorf("expected nil cmd, got %v", cmd)
		}
	})
	if opened != "" {
		t.Errorf("opened %q from a cloud-tunnel run", opened)
	}
	if strings.Contains(out, "App reachable at") || strings.Contains(out, "10.20.30.40") {
		t.Errorf("announced the LAN address for a cloud-tunnel run:\n%s", out)
	}
	if !strings.Contains(out, "Wendy Cloud") {
		t.Errorf("missing the cloud skip notice:\n%s", out)
	}
}

// TestRunPostStartIfReady_CloudNoReportedIPSkipsHook verifies that a cloud
// connection with no reported IP at all skips the postStart hook (there is
// no usable host to dial) instead of trying — and failing — to open the
// asset name, and prints guidance instead.
func TestRunPostStartIfReady_CloudNoReportedIPSkipsHook(t *testing.T) {
	original := browserOpen
	t.Cleanup(func() { browserOpen = original })
	opened := false
	browserOpen = func(string) error {
		opened = true
		return nil
	}

	appCfg := &appconfig.AppConfig{
		AppID: "cloud-app-no-ip",
		Hooks: &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{OpenURL: "http://${WENDY_HOSTNAME}:9999"},
		},
	}
	conn := &grpcclient.AgentConnection{
		Host:         "cctv",
		Reconnect:    neverReconnect,
		AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}},
	}

	out := captureStderr(t, func() {
		if cmd := runPostStartIfReady(context.Background(), context.Background(), conn, appCfg, runOptions{}); cmd != nil {
			t.Errorf("expected nil cmd when no host is reported, got %v", cmd)
		}
	})
	if opened {
		t.Errorf("postStart openURL fired despite no routable device address")
	}
	if !strings.Contains(out, "Skipping postStart hook") {
		t.Errorf("expected guidance notice about the skipped postStart hook; output:\n%s", out)
	}
}

// TestRunPostStartIfReady_CloudReadinessDialsReportedIP verifies that the
// readiness probe itself — not just the postStart hook — targets the
// agent-reported IP for a cloud connection. Before this fix, waitForReadiness
// dialed conn.Host (the unresolvable asset name) and always failed first, so
// the postStart hook logic was never even reached on a cloud run.
func TestRunPostStartIfReady_CloudReadinessDialsReportedIP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()
	port := testPort(t, ln)
	stubLANAddressReachable(t, true) // no agent listens on 127.0.0.1 here

	original := browserOpen
	t.Cleanup(func() { browserOpen = original })
	var opened string
	browserOpen = func(url string) error {
		opened = url
		return nil
	}

	appCfg := &appconfig.AppConfig{
		AppID:     "cloud-readiness-app",
		Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: port}, TimeoutSeconds: 5},
		Hooks: &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{OpenURL: "http://${WENDY_HOSTNAME}:3001"},
		},
	}
	conn := &grpcclient.AgentConnection{
		// A hostname that cannot resolve, standing in for a real cloud asset
		// name — the probe must not dial this directly.
		Host:      "cloud-asset-does-not-resolve.invalid",
		Reconnect: neverReconnect,
		AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
			NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"127.0.0.1"}}},
		}},
	}

	start := time.Now()
	cmd := runPostStartIfReady(context.Background(), context.Background(), conn, appCfg, runOptions{})
	elapsed := time.Since(start)

	if cmd != nil {
		t.Errorf("expected nil cmd for openURL-only hook, got %v", cmd)
	}
	// The real listener answers almost instantly; dialing the unresolvable
	// asset name would instead burn the full 5s TimeoutSeconds.
	if elapsed > 2*time.Second {
		t.Errorf("took %v, expected near-instant readiness against the reported IP (probe likely dialed %q instead)", elapsed, conn.Host)
	}
	if opened != "http://127.0.0.1:3001" {
		t.Errorf("openURL = %q, want the reported-IP URL", opened)
	}
}

// TestRunPostStartIfReady_CloudUnreachableLANIsNeverProbed: when a cloud
// device's LAN address does not answer the quick reachability check, the
// readiness probe must not dial it either — from outside that LAN it only
// burns the whole probe timeout.
func TestRunPostStartIfReady_CloudUnreachableLANIsNeverProbed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()
	dialed := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			dialed <- struct{}{}
			c.Close()
		}
	}()
	stubLANAddressReachable(t, false)

	appCfg := &appconfig.AppConfig{
		AppID:     "cloud-readiness-app",
		Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: testPort(t, ln)}, TimeoutSeconds: 5},
	}
	conn := &grpcclient.AgentConnection{
		Host:      "cloud-asset-does-not-resolve.invalid",
		Reconnect: neverReconnect,
		AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
			NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"127.0.0.1"}}},
		}},
	}

	start := time.Now()
	runPostStartIfReady(context.Background(), context.Background(), conn, appCfg, runOptions{})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v; a cloud run must skip the host-side probe, not wait on it", elapsed)
	}
	select {
	case <-dialed:
		t.Error("probed the agent-reported LAN address after it failed the reachability check")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestRunPostStartIfReady_CloudHooklessAppPrintsNothing is a final-review-fix
// regression test (WDY-2440): an app with no postStart hook, no TCP
// readiness, and no http entitlement has nothing for runPostStartIfReady to
// probe or fire — on a cloud connection, resolveHookHost's isCloud branch
// used to still run (announceReachableURL short-circuits to "" without
// querying the agent when there's no hookURL/port to build a URL from),
// tripping the "no reported IP" guard and printing a "Skipping postStart
// hook" notice for a hook that was never configured. runPostStartIfReady
// must short-circuit before any of that — no output, no browserOpen, no
// agent query at all — mirroring service_lifecycle.go's
// serviceHookRunner.runOne guard (readiness == nil && hooks == nil).
func TestRunPostStartIfReady_CloudHooklessAppPrintsNothing(t *testing.T) {
	original := browserOpen
	t.Cleanup(func() { browserOpen = original })
	opened := false
	browserOpen = func(string) error {
		opened = true
		return nil
	}

	appCfg := &appconfig.AppConfig{
		AppID: "cloud-hookless-app",
		// No Hooks, no Readiness, no http entitlement: nothing to probe or fire.
	}
	agentClient := &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
		NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"10.0.0.9"}}},
	}}
	conn := &grpcclient.AgentConnection{
		Host:         "cctv",
		Reconnect:    neverReconnect,
		AgentService: agentClient,
	}

	out := captureStderr(t, func() {
		if cmd := runPostStartIfReady(context.Background(), context.Background(), conn, appCfg, runOptions{}); cmd != nil {
			t.Errorf("expected nil cmd for a hook-less, readiness-less app, got %v", cmd)
		}
	})
	if opened {
		t.Errorf("browserOpen called for a hook-less app")
	}
	if out != "" {
		t.Errorf("expected no output for a hook-less, readiness-less cloud app, got:\n%s", out)
	}
	if agentClient.calls != 0 {
		t.Errorf("AgentService.GetAgentVersion called %d times, want 0 (nothing to announce or resolve)", agentClient.calls)
	}
}

func TestStartPostStartHook_NilHooks(t *testing.T) {
	cfg := &appconfig.AppConfig{AppID: "test"}
	cmd := startPostStartHook(context.Background(), cfg, "localhost", "")
	if cmd != nil {
		t.Error("expected nil cmd for nil hooks")
	}
}

func TestStartPostStartHook_EmptyCLI(t *testing.T) {
	cfg := &appconfig.AppConfig{
		AppID: "test",
		Hooks: &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{Agent: "echo agent-only"},
		},
	}
	cmd := startPostStartHook(context.Background(), cfg, "localhost", "")
	if cmd != nil {
		t.Error("expected nil cmd when CLI is empty")
	}
}

// TestExpandHookEnv_ServiceName verifies WENDY_SERVICE_NAME expands in both
// Unix and Windows placeholder forms, and expands to the empty string for
// single-container apps (which pass serviceName == "") rather than being left
// verbatim — matching how WENDY_HOSTNAME/WENDY_APP_ID already behave.
func TestExpandHookEnv_ServiceName(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		serviceName string
		want        string
	}{
		{"unix style", "echo ${WENDY_SERVICE_NAME}", "worker", "echo worker"},
		{"windows style", "echo %WENDY_SERVICE_NAME%", "worker", "echo worker"},
		{"empty serviceName expands to empty, unix", "[${WENDY_SERVICE_NAME}]", "", "[]"},
		{"empty serviceName expands to empty, windows", "[%WENDY_SERVICE_NAME%]", "", "[]"},
		{"mixed with hostname and appID", "%WENDY_SERVICE_NAME%@${WENDY_HOSTNAME}/${WENDY_APP_ID}", "worker", "worker@device.local/com.example.app"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expandHookEnv(tc.input, "device.local", "com.example.app", tc.serviceName)
			if got != tc.want {
				t.Errorf("expandHookEnv(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestContextWithPostStartAgentHook(t *testing.T) {
	cfg := &appconfig.AppConfig{
		AppID: "test",
		Hooks: &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{Agent: "wendy-agent utils open-browser http://localhost:3000"},
		},
	}

	ctx := contextWithPostStartAgentHook(context.Background(), cfg)
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("expected outgoing metadata")
	}
	got := md.Get(appconfig.PostStartAgentHookMetadataKey)
	if len(got) != 1 || got[0] != "wendy-agent utils open-browser http://localhost:3000" {
		t.Fatalf("metadata hook = %#v", got)
	}
}

func TestContextWithPostStartAgentHookEmpty(t *testing.T) {
	ctx := contextWithPostStartAgentHook(context.Background(), &appconfig.AppConfig{AppID: "test"})
	if _, ok := metadata.FromOutgoingContext(ctx); ok {
		t.Fatal("expected no outgoing metadata for empty agent hook")
	}
}

func TestEffectiveReadiness_ExplicitReadinessWins(t *testing.T) {
	appCfg := &appconfig.AppConfig{
		Readiness:    &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 1234}},
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: 8080}},
	}
	got := effectiveReadiness(appCfg)
	if got.TCPSocket.Port != 1234 {
		t.Errorf("effectiveReadiness().TCPSocket.Port = %d, want explicit 1234", got.TCPSocket.Port)
	}
}

func TestEffectiveReadiness_SynthesizedFromHTTPEntitlement(t *testing.T) {
	appCfg := &appconfig.AppConfig{
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: 8080}},
	}
	got := effectiveReadiness(appCfg)
	if got == nil || got.TCPSocket == nil || got.TCPSocket.Port != 8080 {
		t.Fatalf("effectiveReadiness() = %+v, want synthesized TCPSocket on port 8080", got)
	}
}

func TestEffectiveReadiness_NoHTTPEntitlementNoReadiness(t *testing.T) {
	appCfg := &appconfig.AppConfig{}
	got := effectiveReadiness(appCfg)
	if got != nil {
		t.Errorf("effectiveReadiness() = %+v, want nil", got)
	}
}

func TestSynthesizedOpenURLHook_PreservesExplicitCommands(t *testing.T) {
	appCfg := &appconfig.AppConfig{
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: 8080}},
		Hooks: &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{
			CLI:   "echo cli",
			Agent: "echo agent",
		}},
	}
	got := synthesizedOpenURLHook(appCfg)
	if got == appCfg.Hooks {
		t.Fatal("synthesizedOpenURLHook returned the original hooks pointer; want a non-mutating copy")
	}
	if got == nil || got.PostStart == nil {
		t.Fatalf("synthesizedOpenURLHook = %+v, want postStart", got)
	}
	if got.PostStart.OpenURL != "http://${WENDY_HOSTNAME}:8080" {
		t.Errorf("OpenURL = %q, want synthesized HTTP URL", got.PostStart.OpenURL)
	}
	if got.PostStart.CLI != "echo cli" || got.PostStart.Agent != "echo agent" {
		t.Errorf("explicit commands were not preserved: %+v", got.PostStart)
	}
	if appCfg.Hooks.PostStart.OpenURL != "" {
		t.Errorf("input hooks mutated: %+v", appCfg.Hooks.PostStart)
	}
}

// TestRunPostStartIfReady_AutoOpensFromHTTPEntitlement verifies the "wendy run
// also opens this page after launch" behavior: an app declaring only an http
// entitlement (no readiness config, no postStart hook) gets its port polled
// for readiness and then opened automatically.
func TestRunPostStartIfReady_AutoOpensFromHTTPEntitlement(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()
	port := testPort(t, ln)

	original := browserOpen
	t.Cleanup(func() { browserOpen = original })
	var opened string
	browserOpen = func(url string) error {
		opened = url
		return nil
	}

	appCfg := &appconfig.AppConfig{
		AppID:        "http-app",
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: port}},
	}
	conn := &grpcclient.AgentConnection{
		Host: "127.0.0.1",
		AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
			NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"127.0.0.1"}}},
		}},
	}

	runPostStartIfReady(context.Background(), context.Background(), conn, appCfg, runOptions{})
	want := fmt.Sprintf("http://127.0.0.1:%d", port)
	if opened != want {
		t.Errorf("opened = %q, want %q", opened, want)
	}
}

func TestRunPostStartIfReady_ProbesReadinessPortButPresentsHTTPPort(t *testing.T) {
	readinessListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start readiness listener: %v", err)
	}
	defer readinessListener.Close()
	readinessPort := testPort(t, readinessListener)

	httpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve HTTP port: %v", err)
	}
	httpPort := testPort(t, httpListener)
	httpListener.Close() // closed: probing this instead would time out

	original := browserOpen
	t.Cleanup(func() { browserOpen = original })
	var opened string
	browserOpen = func(url string) error {
		opened = url
		return nil
	}

	appCfg := &appconfig.AppConfig{
		AppID:        "separate-ports",
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: httpPort}},
		Readiness: &appconfig.ReadinessConfig{
			TCPSocket:      &appconfig.TCPSocketProbe{Port: readinessPort},
			TimeoutSeconds: 1,
		},
	}
	conn := &grpcclient.AgentConnection{
		Host: "127.0.0.1",
		AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
			NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "lo", IpAddresses: []string{"127.0.0.1"}}},
		}},
	}

	start := time.Now()
	out := captureStderr(t, func() {
		runPostStartIfReady(context.Background(), context.Background(), conn, appCfg, runOptions{})
	})
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Errorf("run took %v; explicit readiness port should pass immediately instead of probing the closed HTTP port", elapsed)
	}
	want := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	if opened != want {
		t.Errorf("opened = %q, want HTTP entitlement URL %q", opened, want)
	}
	if !strings.Contains(out, want) {
		t.Errorf("announcement does not use HTTP entitlement port; output:\n%s", out)
	}
}

// TestRunPostStartIfReady_ExplicitHookNotOverriddenByHTTPEntitlement verifies
// that an app declaring both an http entitlement AND an explicit
// hooks.postStart.openURL keeps the explicit URL — the entitlement only fills
// in when nothing is configured.
func TestRunPostStartIfReady_ExplicitHookNotOverriddenByHTTPEntitlement(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()
	port := testPort(t, ln)

	original := browserOpen
	t.Cleanup(func() { browserOpen = original })
	var opened string
	browserOpen = func(url string) error {
		opened = url
		return nil
	}

	appCfg := &appconfig.AppConfig{
		AppID:        "http-app-explicit",
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: port}},
		Hooks: &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{OpenURL: "http://${WENDY_HOSTNAME}:9999/custom"},
		},
	}
	// The explicit openURL hook makes announceReachableURL query the agent
	// regardless of the http entitlement (same as pre-existing hook-only
	// tests above); AgentService must be non-nil or that call panics.
	conn := &grpcclient.AgentConnection{
		Host:         "127.0.0.1",
		AgentService: &fakeAgentVersionClient{err: errors.New("agent unreachable")},
	}

	runPostStartIfReady(context.Background(), context.Background(), conn, appCfg, runOptions{})
	if opened != "http://127.0.0.1:9999/custom" {
		t.Errorf("opened = %q, want the explicit hook URL unchanged", opened)
	}
}

func TestDefaultPostStartBrowserAllowed(t *testing.T) {
	previousInteractive, previousJSON := isInteractiveTerminalFn, jsonOutput
	t.Cleanup(func() { isInteractiveTerminalFn, jsonOutput = previousInteractive, previousJSON })
	for _, tc := range []struct {
		interactive, json, want bool
	}{
		{interactive: true, json: false, want: true},
		{interactive: true, json: true, want: false},
		{interactive: false, json: false, want: false},
		{interactive: false, json: true, want: false},
	} {
		isInteractiveTerminalFn = func() bool { return tc.interactive }
		jsonOutput = tc.json
		if got := defaultPostStartBrowserAllowed(); got != tc.want {
			t.Errorf("interactive=%v json=%v: allowed = %v, want %v", tc.interactive, tc.json, got, tc.want)
		}
	}
}

func TestStartPostStartHook_NoBrowserWithoutInteractiveTerminal(t *testing.T) {
	previousAllowed, previousOpen := postStartBrowserAllowed, browserOpen
	t.Cleanup(func() { postStartBrowserAllowed, browserOpen = previousAllowed, previousOpen })
	postStartBrowserAllowed = func() bool { return false }
	opened := false
	browserOpen = func(string) error { opened = true; return nil }
	cfg := &appconfig.AppConfig{AppID: "app", Hooks: &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{OpenURL: "http://${WENDY_HOSTNAME}:8080"}}}
	out := captureStderr(t, func() { startPostStartHook(context.Background(), cfg, "192.168.1.20", "") })
	if opened {
		t.Fatal("opened a browser without an interactive terminal")
	}
	if !strings.Contains(out, "http://192.168.1.20:8080") {
		t.Fatalf("the URL was not printed instead: %q", out)
	}
}
