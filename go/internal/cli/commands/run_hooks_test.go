package commands

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
	err := waitForReadiness(context.Background(), nil, "localhost", 0)
	if err != nil {
		t.Fatalf("expected nil error for nil config, got %v", err)
	}
}

func TestWaitForReadiness_NilTCPSocket(t *testing.T) {
	cfg := &appconfig.ReadinessConfig{}
	err := waitForReadiness(context.Background(), cfg, "localhost", 0)
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
	err = waitForReadiness(context.Background(), cfg, "127.0.0.1", 0)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v, expected near-instant for already listening port", elapsed)
	}
}

func TestWaitForReadiness_HTTPProbeCompletesRequest(t *testing.T) {
	requests := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(context.Background())
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	port := testPort(t, server.Listener)
	cfg := &appconfig.ReadinessConfig{
		TCPSocket:      &appconfig.TCPSocketProbe{Port: port},
		TimeoutSeconds: 5,
	}
	if err := waitForReadiness(context.Background(), cfg, "127.0.0.1", port); err != nil {
		t.Fatalf("HTTP readiness failed: %v", err)
	}

	select {
	case req := <-requests:
		if req.Method != http.MethodHead {
			t.Errorf("readiness method = %q, want HEAD", req.Method)
		}
		if !req.Close {
			t.Error("HTTP readiness must request connection close")
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP readiness did not reach the server")
	}
}

func TestCloudHTTPReadinessPort(t *testing.T) {
	const port = 8000
	readiness := &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: port}}
	appCfg := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{
		{Type: appconfig.EntitlementHTTP, Port: port},
	}}
	cloud := &grpcclient.AgentConnection{Reconnect: neverReconnect}
	if got := cloudHTTPReadinessPort(cloud, appCfg, readiness); got != port {
		t.Errorf("cloud HTTP readiness port = %d, want %d", got, port)
	}
	if got := cloudHTTPReadinessPort(&grpcclient.AgentConnection{}, appCfg, readiness); got != 0 {
		t.Errorf("direct connection HTTP readiness port = %d, want 0", got)
	}
	readiness.TCPSocket.Port = 9000
	if got := cloudHTTPReadinessPort(cloud, appCfg, readiness); got != 0 {
		t.Errorf("mismatched explicit readiness port = %d, want 0", got)
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
	err = waitForReadiness(context.Background(), cfg, "127.0.0.1", 0)
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
	err = waitForReadiness(context.Background(), cfg, "127.0.0.1", 0)
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
	err = waitForReadiness(ctx, cfg, "127.0.0.1", 0)
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
// agent reports; an active desktop mesh VPN supplies a cloud route; an
// IPv6-literal Host swaps in the agent-reported IP when one is available; and
// a cloud connection without active mesh DNS reports ok=false without trusting
// an agent-reported LAN address.
func TestResolveHookHost(t *testing.T) {
	originalLookup := osLookupHostFn
	t.Cleanup(func() { osLookupHostFn = originalLookup })
	osLookupHostFn = func(_ context.Context, host string) ([]string, error) {
		if host == "device-42.mesh.wendy.internal" {
			return []string{"10.99.0.42"}, nil
		}
		return nil, errors.New("mesh DNS unavailable")
	}

	appCfgWithHook := func() *appconfig.AppConfig {
		return &appconfig.AppConfig{
			AppID: "resolve-host-app",
			Hooks: &appconfig.HooksConfig{
				PostStart: &appconfig.HookCommand{OpenURL: "http://${WENDY_HOSTNAME}:80"},
			},
		}
	}

	cases := []struct {
		name     string
		conn     *grpcclient.AgentConnection
		wantHost string
		wantOK   bool
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
			name: "active mesh VPN preferred over cloud-reported IP",
			conn: &grpcclient.AgentConnection{
				Host:      "cctv",
				MeshHost:  "device-42.mesh.wendy.internal",
				Reconnect: neverReconnect,
				AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
					NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"10.10.10.10"}}},
				}},
			},
			wantHost: "device-42.mesh.wendy.internal",
			wantOK:   true,
		},
		{
			name: "inactive mesh VPN rejects cloud-reported LAN IP",
			conn: &grpcclient.AgentConnection{
				Host:      "cctv",
				MeshHost:  "device-43.mesh.wendy.internal",
				Reconnect: neverReconnect,
				AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
					NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"10.10.10.10"}}},
				}},
			},
			wantHost: "",
			wantOK:   false,
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
			host, ok := resolveHookHost(context.Background(), tc.conn, appCfgWithHook())
			if host != tc.wantHost || ok != tc.wantOK {
				t.Errorf("resolveHookHost() = (%q, %v), want (%q, %v)", host, ok, tc.wantHost, tc.wantOK)
			}
		})
	}
}

// TestRunPostStartIfReady_ActiveMeshVPNUsesMeshHostname covers cloud runs where
// the agent's LAN IP is absent or unreachable. Resolving the stable mesh name
// into 10.99/16 proves the desktop VPN is active, so the host-side postStart
// hook must fire with that name rather than being suppressed.
func TestRunPostStartIfReady_ActiveMeshVPNUsesMeshHostname(t *testing.T) {
	originalLookup := osLookupHostFn
	originalOpen := browserOpen
	t.Cleanup(func() {
		osLookupHostFn = originalLookup
		browserOpen = originalOpen
	})
	osLookupHostFn = func(_ context.Context, host string) ([]string, error) {
		if host != "device-215.mesh.wendy.internal" {
			t.Fatalf("mesh lookup host = %q, want device-215.mesh.wendy.internal", host)
		}
		return []string{"10.99.0.215"}, nil
	}
	var opened string
	browserOpen = func(url string) error {
		opened = url
		return nil
	}

	appCfg := &appconfig.AppConfig{
		AppID: "cloud-mesh-app",
		Hooks: &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{OpenURL: "http://${WENDY_HOSTNAME}:9999"},
		},
	}
	agentClient := &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}}
	conn := &grpcclient.AgentConnection{
		Host:         "cctv",
		MeshHost:     "device-215.mesh.wendy.internal",
		Reconnect:    neverReconnect,
		AgentService: agentClient,
	}

	if cmd := runPostStartIfReady(context.Background(), context.Background(), conn, appCfg, runOptions{}); cmd != nil {
		t.Errorf("expected nil cmd for openURL-only hook, got %v", cmd)
	}
	if opened != "http://device-215.mesh.wendy.internal:9999" {
		t.Errorf("openURL = %q, want mesh-routed URL", opened)
	}
	if agentClient.calls != 0 {
		t.Errorf("GetAgentVersion calls = %d, want 0 when active mesh DNS already supplies the route", agentClient.calls)
	}
}

// TestRunPostStartIfReady_CloudLANIPRejected verifies that a cloud tunnel does
// not make the device's reported LAN address reachable from the developer's
// machine. Without an active mesh route, the host-side postStart hook is
// skipped instead of opening that private address.
func TestRunPostStartIfReady_CloudLANIPRejected(t *testing.T) {
	original := browserOpen
	t.Cleanup(func() { browserOpen = original })
	var opened string
	browserOpen = func(url string) error {
		opened = url
		return nil
	}

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
	if opened != "" {
		t.Errorf("openURL = %q, want no browser open without an active mesh route", opened)
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

// TestRunPostStartIfReady_CloudReadinessRejectsReportedIP verifies that the
// readiness probe does not treat an agent-reported address as reachable merely
// because the control connection uses a cloud tunnel.
func TestRunPostStartIfReady_CloudReadinessRejectsReportedIP(t *testing.T) {
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
	// Neither the real listener nor the asset name should be dialed; without a
	// verified mesh route the cloud readiness check is skipped immediately.
	if elapsed > 2*time.Second {
		t.Errorf("took %v, expected cloud readiness to skip immediately", elapsed)
	}
	if opened != "" {
		t.Errorf("openURL = %q, want no browser open without an active mesh route", opened)
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
