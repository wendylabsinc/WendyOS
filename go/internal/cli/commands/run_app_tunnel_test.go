package commands

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// tunnelStarterCall records one startCloudAppTunnel call.
type tunnelStarterCall struct {
	remotePort int
	ctx        context.Context
}

// swapCloudAppTunnel replaces startCloudAppTunnel with start and records
// every call. A nil start fails the test on any call: the path under test
// must not open a cloud app tunnel.
func swapCloudAppTunnel(t *testing.T, start func(ctx context.Context, conn *grpcclient.AgentConnection, remotePort int) (*cloudAppTunnel, error)) *[]tunnelStarterCall {
	t.Helper()
	original := startCloudAppTunnel
	t.Cleanup(func() { startCloudAppTunnel = original })
	var mu sync.Mutex
	var calls []tunnelStarterCall
	startCloudAppTunnel = func(ctx context.Context, conn *grpcclient.AgentConnection, remotePort int) (*cloudAppTunnel, error) {
		mu.Lock()
		calls = append(calls, tunnelStarterCall{remotePort: remotePort, ctx: ctx})
		mu.Unlock()
		if start == nil {
			t.Errorf("opened a cloud app tunnel to port %d; this path must not", remotePort)
			return nil, errors.New("unexpected tunnel")
		}
		return start(ctx, conn, remotePort)
	}
	return &calls
}

// fixedTunnel is an injected starter whose tunnel listens on localPort and
// whose relays reach localPort too: the app stands in at the tunnel's local
// end.
func fixedTunnel(localPort int) func(context.Context, *grpcclient.AgentConnection, int) (*cloudAppTunnel, error) {
	return func(_ context.Context, _ *grpcclient.AgentConnection, remotePort int) (*cloudAppTunnel, error) {
		target := net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))
		return &cloudAppTunnel{LocalPort: localPort, RemotePort: remotePort, relay: func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", target)
		}}, nil
	}
}

// tunnelFakeContainerClient is lifecycleFakeContainerClient plus the agent's
// GetContainerPorts. With ports nil it answers Unimplemented, as an agent
// from before that call does.
type tunnelFakeContainerClient struct {
	lifecycleFakeContainerClient

	mu    sync.Mutex
	ports func(*agentpb.GetContainerPortsRequest) []*agentpb.PortEntry
	calls int
}

func (f *tunnelFakeContainerClient) GetContainerPorts(_ context.Context, req *agentpb.GetContainerPortsRequest, _ ...grpc.CallOption) (*agentpb.GetContainerPortsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.ports == nil {
		return nil, status.Error(codes.Unimplemented, "unknown method GetContainerPorts")
	}
	return &agentpb.GetContainerPortsResponse{Ports: f.ports(req)}, nil
}

func (f *tunnelFakeContainerClient) portCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// offNetworkCloudConn is a cloud connection whose device reports a LAN
// address this machine cannot reach, with no Wendy Mesh: the case the cloud
// app tunnel exists for.
func offNetworkCloudConn(t *testing.T) *grpcclient.AgentConnection {
	t.Helper()
	stubLANAddressReachable(t, false)
	return &grpcclient.AgentConnection{
		Host:             "Dev Kit",
		Reconnect:        neverReconnect,
		ContainerService: &tunnelFakeContainerClient{},
		AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
			NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"10.20.30.40"}}},
		}},
	}
}

// headServer stands in for a running app at the tunnel's local end. It
// answers every request, counting the readiness probe's HEADs.
func headServer(t *testing.T) (port int, heads func() int) {
	t.Helper()
	var mu sync.Mutex
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			mu.Lock()
			count++
			mu.Unlock()
		}
		io.WriteString(w, "app")
	}))
	t.Cleanup(server.Close)
	return testPort(t, server.Listener), func() int {
		mu.Lock()
		defer mu.Unlock()
		return count
	}
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := testPort(t, ln)
	ln.Close()
	return port
}

// appBehindCloud is a fake cloud port dialer: every tunnel connection reaches
// target, as the relay reaches the app's port on the device.
func appBehindCloud(target string, dialed *[]int, mu *sync.Mutex) func(context.Context, int) (net.Conn, error) {
	return func(ctx context.Context, port int) (net.Conn, error) {
		mu.Lock()
		*dialed = append(*dialed, port)
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, "tcp", target)
	}
}

func getBody(t *testing.T, rawURL string) string {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	resp, err := client.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func TestOpenCloudAppTunnelKeepsAppPortWhenFree(t *testing.T) {
	appPort, _ := headServer(t)
	port := freeLoopbackPort(t)
	var mu sync.Mutex
	var dialed []int
	conn := &grpcclient.AgentConnection{Host: "Dev Kit", Reconnect: neverReconnect, RegistryDialer: appBehindCloud(net.JoinHostPort("127.0.0.1", strconv.Itoa(appPort)), &dialed, &mu)}

	tunnel, err := openCloudAppTunnel(context.Background(), conn, port)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.Close()
	if tunnel.LocalPort != port || tunnel.RemotePort != port {
		t.Fatalf("tunnel = localhost:%d → %d, want the app's own port %d on both ends", tunnel.LocalPort, tunnel.RemotePort, port)
	}
	if body := getBody(t, fmt.Sprintf("http://localhost:%d/", port)); body != "app" {
		t.Fatalf("body through tunnel = %q, want the app's", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) != 1 || dialed[0] != port {
		t.Fatalf("cloud dialer asked for ports %v, want [%d]", dialed, port)
	}
}

func TestOpenCloudAppTunnelFallsBackWhenPortInUse(t *testing.T) {
	for _, network := range []struct{ name, addr string }{{"ipv4 loopback", "127.0.0.1:0"}, {"ipv6 loopback", "[::1]:0"}} {
		t.Run(network.name, func(t *testing.T) {
			busy, err := net.Listen("tcp", network.addr)
			if err != nil {
				t.Skipf("cannot listen on %s: %v", network.addr, err)
			}
			defer busy.Close()
			go func() {
				for {
					c, err := busy.Accept()
					if err != nil {
						return
					}
					c.Close()
				}
			}()
			port := testPort(t, busy)
			appPort, _ := headServer(t)
			var mu sync.Mutex
			var dialed []int
			conn := &grpcclient.AgentConnection{Reconnect: neverReconnect, RegistryDialer: appBehindCloud(net.JoinHostPort("127.0.0.1", strconv.Itoa(appPort)), &dialed, &mu)}

			tunnel, err := openCloudAppTunnel(context.Background(), conn, port)
			if err != nil {
				t.Fatal(err)
			}
			defer tunnel.Close()
			if tunnel.LocalPort == port || tunnel.LocalPort == 0 || tunnel.RemotePort != port {
				t.Fatalf("tunnel = localhost:%d → %d, want a free local port forwarding to %d", tunnel.LocalPort, tunnel.RemotePort, port)
			}
			if body := getBody(t, fmt.Sprintf("http://127.0.0.1:%d/", tunnel.LocalPort)); body != "app" {
				t.Fatalf("body through tunnel = %q, want the app's", body)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(dialed) != 1 || dialed[0] != port {
				t.Fatalf("cloud dialer asked for ports %v, want the device port [%d]", dialed, port)
			}
		})
	}
}

func TestOpenCloudAppTunnelClosesOnContextCancel(t *testing.T) {
	// The "app" holds each connection open until the client goes away.
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	go func() {
		for {
			c, err := app.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); c.Close() }()
		}
	}()
	var mu sync.Mutex
	var dialed []int
	conn := &grpcclient.AgentConnection{Reconnect: neverReconnect, RegistryDialer: appBehindCloud(app.Addr().String(), &dialed, &mu)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tunnel, err := openCloudAppTunnel(ctx, conn, freeLoopbackPort(t))
	if err != nil {
		t.Fatal(err)
	}
	local := net.JoinHostPort("127.0.0.1", strconv.Itoa(tunnel.LocalPort))
	inFlight, err := net.Dial("tcp", local)
	if err != nil {
		t.Fatal(err)
	}
	defer inFlight.Close()
	if _, err := inFlight.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}

	cancel()
	select {
	case <-tunnel.done:
	case <-time.After(5 * time.Second):
		t.Fatal("tunnel still accepting 5s after its context ended")
	}
	if tunnel.open() {
		t.Fatal("open() = true after the context ended")
	}
	if c, err := net.DialTimeout("tcp", local, time.Second); err == nil {
		c.Close()
		t.Fatalf("%s still accepts connections after the run ended", local)
	}
	// The forwarded connection is closed too: a read ends promptly, not at
	// the deadline.
	_ = inFlight.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = bufio.NewReader(inFlight).ReadByte()
	var netErr net.Error
	if err == nil || (errors.As(err, &netErr) && netErr.Timeout()) {
		t.Fatalf("forwarded connection still open after the run ended: %v", err)
	}
	tunnel.Close() // idempotent after the context ended
}

func TestOpenCloudAppTunnelRequiresCloudDialer(t *testing.T) {
	if _, err := openCloudAppTunnel(context.Background(), &grpcclient.AgentConnection{Reconnect: neverReconnect}, 3005); err == nil {
		t.Fatal("opened a tunnel on a connection with no cloud port dialer")
	}
}

func TestCloudAppTunnelPort(t *testing.T) {
	readiness := func(port int) *appconfig.ReadinessConfig {
		return &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: port}}
	}
	hooks := func(h appconfig.HookCommand) *appconfig.HooksConfig { return &appconfig.HooksConfig{PostStart: &h} }
	httpPort := func(port int) []appconfig.Entitlement {
		return []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: port}}
	}
	for _, tc := range []struct {
		name string
		cfg  *appconfig.AppConfig
		want int
	}{
		{"nil config", nil, 0},
		{"openURL port", &appconfig.AppConfig{Hooks: hooks(appconfig.HookCommand{OpenURL: "http://${WENDY_HOSTNAME}:3005/view"})}, 3005},
		{"openURL https default port", &appconfig.AppConfig{Hooks: hooks(appconfig.HookCommand{OpenURL: "https://${WENDY_HOSTNAME}/"})}, 443},
		{"openURL wins over http entitlement", &appconfig.AppConfig{Entitlements: httpPort(8080), Hooks: hooks(appconfig.HookCommand{OpenURL: "http://${WENDY_HOSTNAME}:9000"})}, 9000},
		{"http entitlement synthesizes openURL", &appconfig.AppConfig{Entitlements: httpPort(8080)}, 8080},
		{"cli hook uses readiness port", &appconfig.AppConfig{Readiness: readiness(3005), Hooks: hooks(appconfig.HookCommand{CLI: "wendy utils open-browser http://${WENDY_HOSTNAME}:3005"})}, 3005},
		{"device hostname only in query", &appconfig.AppConfig{Entitlements: httpPort(8765), Hooks: hooks(appconfig.HookCommand{OpenURL: "https://viewer.example/?ws=ws://${WENDY_HOSTNAME}:8765"})}, 8765},
		{"readiness alone is not forwarded", &appconfig.AppConfig{Readiness: readiness(6379)}, 0},
		{"cli hook not addressing the device", &appconfig.AppConfig{Readiness: readiness(3005), Hooks: hooks(appconfig.HookCommand{CLI: "say ready"})}, 0},
		{"cli hook with no known port", &appconfig.AppConfig{Hooks: hooks(appconfig.HookCommand{CLI: "open http://${WENDY_HOSTNAME}"})}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cloudAppTunnelPort(tc.cfg); got != tc.want {
				t.Fatalf("cloudAppTunnelPort = %d, want %d", got, tc.want)
			}
		})
	}
}

// An off-network cloud run forwards the app's port on the same local port,
// probes readiness through it, and opens the browser on localhost.
func TestRunPostStartIfReadyCloudForwardsThroughTunnel(t *testing.T) {
	port, heads := headServer(t)
	calls := swapCloudAppTunnel(t, fixedTunnel(port))
	opened := swapBrowserOpen(t)
	conn := offNetworkCloudConn(t)
	cfg := &appconfig.AppConfig{
		AppID:     "camera-feed",
		Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: port}, TimeoutSeconds: 5},
		Hooks:     &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{OpenURL: fmt.Sprintf("http://${WENDY_HOSTNAME}:%d/view", port)}},
	}
	hookCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := captureStderr(t, func() {
		runPostStartIfReady(context.Background(), hookCtx, conn, cfg, runOptions{})
	})
	want := fmt.Sprintf("http://localhost:%d/view", port)
	if len(*opened) != 1 || (*opened)[0] != want {
		t.Fatalf("browser opened %v, want [%s]", *opened, want)
	}
	if len(*calls) != 1 || (*calls)[0].remotePort != port || (*calls)[0].ctx != hookCtx {
		t.Fatalf("tunnel starts = %+v, want one to port %d tied to the hook context", *calls, port)
	}
	if heads() == 0 {
		t.Fatal("readiness was not probed through the tunnel")
	}
	for _, line := range []string{
		fmt.Sprintf("Forwarding http://localhost:%d → camera-feed on Dev Kit through Wendy Cloud (closes when wendy run exits)", port),
		"App reachable at " + want,
		fmt.Sprintf("Waiting for localhost:%d to be ready", port),
	} {
		if !strings.Contains(out, line) {
			t.Errorf("missing %q in output:\n%s", line, out)
		}
	}
	if strings.Contains(out, "10.20.30.40") || strings.Contains(out, "Connect the desktop VPN") {
		t.Errorf("announced the unreachable LAN address or the skip notice:\n%s", out)
	}
}

// When the app's port is busy here, the tunnel listens elsewhere and the
// openURL follows it, keeping scheme, path and query.
func TestRunPostStartIfReadyCloudTunnelFallbackPortRewritesOpenURL(t *testing.T) {
	local, heads := headServer(t)
	swapCloudAppTunnel(t, fixedTunnel(local))
	opened := swapBrowserOpen(t)
	conn := offNetworkCloudConn(t)
	cfg := &appconfig.AppConfig{
		AppID:     "camera-feed",
		Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 3005}, TimeoutSeconds: 5},
		Hooks:     &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{OpenURL: "https://${WENDY_HOSTNAME}:3005/view?app=${WENDY_APP_ID}"}},
	}

	out := captureStderr(t, func() {
		runPostStartIfReady(context.Background(), context.Background(), conn, cfg, runOptions{})
	})
	want := fmt.Sprintf("https://localhost:%d/view?app=camera-feed", local)
	if len(*opened) != 1 || (*opened)[0] != want {
		t.Fatalf("browser opened %v, want [%s]", *opened, want)
	}
	if heads() == 0 {
		t.Fatal("readiness was not probed at the tunnel's local port")
	}
	if !strings.Contains(out, fmt.Sprintf("Forwarding https://localhost:%d → camera-feed (port 3005) on Dev Kit through Wendy Cloud; port 3005 is in use on this machine", local)) {
		t.Errorf("missing the fallback-port forwarding line:\n%s", out)
	}
}

// The camera-feed-yolo shape: a cli hook opening http://${WENDY_HOSTNAME}:3005
// runs against localhost when the tunnel kept port 3005.
func TestServiceHookRunnerCloudTunnelRunsCLIHookOnSamePort(t *testing.T) {
	swapCloudAppTunnel(t, fixedTunnel(3005))
	execs := swapPostStartExec(t)
	conn := offNetworkCloudConn(t)
	r := &serviceHookRunner{conn: conn, opts: runOptions{hostReadinessConfirmed: true}}
	cfg := &appconfig.AppConfig{
		AppID:     "camera-feed-yolo",
		Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 3005}},
		Hooks:     &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{CLI: "wendy utils open-browser http://${WENDY_HOSTNAME}:3005"}},
	}

	captureStderr(t, func() { r.runOne(context.Background(), context.Background(), cfg) })
	if len(*execs) != 1 || (*execs)[0] != "wendy utils open-browser http://localhost:3005" {
		t.Fatalf("cli hooks run = %q, want the hook against localhost", *execs)
	}
}

// A cli hook is shell text, so when the tunnel had to move ports the run
// skips it and prints the local URL instead of rewriting the command.
func TestServiceHookRunnerCloudTunnelSkipsCLIHookOnOtherPort(t *testing.T) {
	swapCloudAppTunnel(t, fixedTunnel(54321))
	execs := swapPostStartExec(t)
	conn := offNetworkCloudConn(t)
	r := &serviceHookRunner{conn: conn, opts: runOptions{hostReadinessConfirmed: true}}
	cfg := &appconfig.AppConfig{
		AppID:     "camera-feed-yolo",
		Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 3005}},
		Hooks:     &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{CLI: "wendy utils open-browser http://${WENDY_HOSTNAME}:3005"}},
	}

	out := captureStderr(t, func() { r.runOne(context.Background(), context.Background(), cfg) })
	if len(*execs) != 0 {
		t.Fatalf("ran cli hook %q against the wrong port", *execs)
	}
	if !strings.Contains(out, "Skipping the postStart cli hook") || !strings.Contains(out, "http://localhost:54321") {
		t.Fatalf("missing the skip notice with the local URL:\n%s", out)
	}
}

// When the tunnel cannot start, the run falls back to today's skip notice
// plus the exact command to forward the port by hand.
func TestRunPostStartIfReadyCloudTunnelFailureFallsBack(t *testing.T) {
	swapCloudAppTunnel(t, func(context.Context, *grpcclient.AgentConnection, int) (*cloudAppTunnel, error) {
		return nil, errors.New("listening on 127.0.0.1: no ports left")
	})
	opened := swapBrowserOpen(t)
	execs := swapPostStartExec(t)
	conn := offNetworkCloudConn(t)
	cfg := &appconfig.AppConfig{
		AppID:     "camera-feed-yolo",
		Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 3005}, TimeoutSeconds: 5},
		Hooks:     &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{CLI: "wendy utils open-browser http://${WENDY_HOSTNAME}:3005"}},
	}

	start := time.Now()
	out := captureStderr(t, func() {
		if cmd := runPostStartIfReady(context.Background(), context.Background(), conn, cfg, runOptions{}); cmd != nil {
			t.Errorf("started a hook after the tunnel failed: %v", cmd)
		}
	})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %v; a failed tunnel must skip readiness, not wait on it", elapsed)
	}
	if len(*opened) != 0 || len(*execs) != 0 {
		t.Fatalf("ran hooks after the tunnel failed: browser=%v cli=%v", *opened, *execs)
	}
	for _, line := range []string{
		"Could not open a Wendy Cloud tunnel to camera-feed-yolo port 3005: listening on 127.0.0.1: no ports left",
		"Connect the desktop VPN to reach this cloud device.",
		`wendy cloud tunnel 3005:3005 --device "Dev Kit"`,
		"http://localhost:3005",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("missing %q in output:\n%s", line, out)
		}
	}
}

// A detached run cannot keep a tunnel open, so it prints the command instead;
// JSON output (the MCP run tool's) is unchanged.
func TestDetachedRunCloudWithoutMeshPrintsTunnelCommand(t *testing.T) {
	swapCloudAppTunnel(t, nil)
	conn := &grpcclient.AgentConnection{Host: "Dev Kit", Reconnect: neverReconnect}
	cfg := &appconfig.AppConfig{AppID: "web", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: 8080}}}

	t.Run("text", func(t *testing.T) {
		isolateDetachedOutput(t)
		jsonOutput = false
		stdout, stderr := captureBoth(t, func() {
			if err := (runOptions{detachedOutput: true}).reportDetachedRun(context.Background(), conn, "web", cfg, cfg); err != nil {
				t.Fatal(err)
			}
		})
		want := `To open web from this machine, run wendy cloud tunnel 8080:8080 --device "Dev Kit" and open http://localhost:8080`
		if stdout != "" || !strings.Contains(stderr, want) || strings.Count(stderr, "wendy cloud tunnel") != 1 {
			t.Fatalf("want one hint %q; stdout=%q stderr=%q", want, stdout, stderr)
		}
	})
	t.Run("json", func(t *testing.T) {
		isolateDetachedOutput(t)
		stdout, stderr := captureBoth(t, func() {
			if err := (runOptions{detachedOutput: true}).reportDetachedRun(context.Background(), conn, "web", cfg); err != nil {
				t.Fatal(err)
			}
		})
		if result := decodeDetachedResult(t, stdout); result.URL != "" || len(result.Endpoints) != 0 {
			t.Fatalf("JSON result changed: %+v", result)
		}
		if strings.Contains(stdout+stderr, "wendy cloud tunnel") {
			t.Fatalf("JSON run printed the tunnel hint: stdout=%q stderr=%q", stdout, stderr)
		}
	})
}

// Paths with a route to the device never open a tunnel: a reachable LAN
// address, an active Wendy Mesh, and a direct connection.
func TestResolveHookRouteLeavesRoutedTargetsAlone(t *testing.T) {
	swapCloudAppTunnel(t, nil)
	cfg := &appconfig.AppConfig{AppID: "web", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: 8080}}}
	agent := func() *fakeAgentVersionClient {
		return &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{
			NetworkInterfaces: []*agentpb.NetworkInterface{{Name: "eth0", IpAddresses: []string{"10.20.30.40"}}},
		}}
	}

	t.Run("reachable LAN", func(t *testing.T) {
		stubLANAddressReachable(t, true)
		conn := &grpcclient.AgentConnection{Host: "Dev Kit", Reconnect: neverReconnect, AgentService: agent()}
		var route hookRoute
		var ok bool
		captureStderr(t, func() {
			route, ok = resolveHookRoute(context.Background(), context.Background(), conn, cfg, nil, false)
		})
		if !ok || route.host != "10.20.30.40" || route.tunnel != nil {
			t.Fatalf("route = %+v ok=%v, want the LAN address", route, ok)
		}
	})
	t.Run("mesh", func(t *testing.T) {
		originalLookup := osLookupHostFn
		t.Cleanup(func() { osLookupHostFn = originalLookup })
		osLookupHostFn = func(context.Context, string) ([]string, error) { return []string{"10.99.0.7"}, nil }
		conn := &grpcclient.AgentConnection{Host: "Dev Kit", MeshHost: "device-7.mesh.wendy.internal", Reconnect: neverReconnect, AgentService: agent()}
		var route hookRoute
		var ok bool
		captureStderr(t, func() {
			route, ok = resolveHookRoute(context.Background(), context.Background(), conn, cfg, nil, false)
		})
		if !ok || route.host != conn.MeshHost || route.tunnel != nil {
			t.Fatalf("route = %+v ok=%v, want the mesh host", route, ok)
		}
	})
	t.Run("direct", func(t *testing.T) {
		conn := &grpcclient.AgentConnection{Host: "dev.local", AgentService: agent()}
		var route hookRoute
		var ok bool
		captureStderr(t, func() {
			route, ok = resolveHookRoute(context.Background(), context.Background(), conn, cfg, nil, false)
		})
		if !ok || route.host != "dev.local" || route.tunnel != nil {
			t.Fatalf("route = %+v ok=%v, want the direct host", route, ok)
		}
	})
}

func TestAppTunnelSetReusesOpenTunnelPerPort(t *testing.T) {
	var tunnels []*cloudAppTunnel
	calls := swapCloudAppTunnel(t, func(_ context.Context, _ *grpcclient.AgentConnection, remotePort int) (*cloudAppTunnel, error) {
		tunnel := &cloudAppTunnel{LocalPort: remotePort, RemotePort: remotePort, done: make(chan struct{})}
		tunnel.stop = sync.OnceFunc(func() { close(tunnel.done) })
		tunnels = append(tunnels, tunnel)
		return tunnel, nil
	})
	var set appTunnelSet
	first, opened, err := set.forward(context.Background(), nil, 3005)
	if err != nil || !opened {
		t.Fatalf("first forward: opened=%v err=%v", opened, err)
	}
	again, opened, err := set.forward(context.Background(), nil, 3005)
	if err != nil || opened || again != first {
		t.Fatalf("second forward to the same port: opened=%v err=%v same=%v; want the open tunnel reused", opened, err, again == first)
	}
	if _, opened, _ := set.forward(context.Background(), nil, 8080); !opened {
		t.Fatal("a second port did not get its own tunnel")
	}
	first.Close()
	if _, opened, _ := set.forward(context.Background(), nil, 3005); !opened {
		t.Fatal("a closed tunnel was reused")
	}
	if len(*calls) != 3 {
		t.Fatalf("tunnel starts = %d, want 3", len(*calls))
	}
	set.closeAll()
	for _, tunnel := range tunnels {
		if tunnel.open() {
			t.Fatalf("closeAll left localhost:%d open", tunnel.LocalPort)
		}
	}
}

// A watch session keeps one tunnel across deploys: a deploy whose readiness
// failed releases the host lifecycle, and the retry reuses the session's
// tunnel instead of opening another.
func TestRunPostStartIfReadyWatchKeepsOneTunnel(t *testing.T) {
	dead := freeLoopbackPort(t) // nothing answers: readiness fails
	calls := swapCloudAppTunnel(t, fixedTunnel(dead))
	swapBrowserOpen(t)
	conn := offNetworkCloudConn(t)
	opts := withWatchInvariants(runOptions{readinessTimeout: 300 * time.Millisecond})
	session, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts.watchState.setSessionContext(session)
	cfg := &appconfig.AppConfig{
		AppID:     "web",
		Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 3005}},
		Hooks:     &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{OpenURL: "http://${WENDY_HOSTNAME}:3005"}},
	}

	out := captureStderr(t, func() {
		for range 2 {
			cycle, cycleCancel := context.WithCancel(context.Background())
			runPostStartIfReady(cycle, opts.watchState.hookContext(cycle), conn, cfg, opts)
			cycleCancel()
		}
	})
	if len(*calls) != 1 || (*calls)[0].ctx != session {
		t.Fatalf("tunnel starts = %+v, want one tied to the watch session", *calls)
	}
	if strings.Count(out, "Forwarding ") != 1 || !strings.Contains(out, "(closes when the watch stops)") {
		t.Fatalf("want one forwarding line for the session:\n%s", out)
	}
	if strings.Count(out, "Waiting for localhost:") != 2 {
		t.Fatalf("the second deploy did not retry readiness through the same tunnel:\n%s", out)
	}
}

// The whole attached path against a real tunnel: readiness and the browser
// go through it, and the run's end closes it.
func TestServiceHookRunnerCloudTunnelEndToEndClosesWithRun(t *testing.T) {
	swapBrowserOpen(t)
	appPort, heads := headServer(t)
	port := freeLoopbackPort(t)
	var mu sync.Mutex
	var dialed []int
	conn := offNetworkCloudConn(t)
	conn.RegistryDialer = appBehindCloud(net.JoinHostPort("127.0.0.1", strconv.Itoa(appPort)), &dialed, &mu)
	r := &serviceHookRunner{conn: conn}
	cfg := &appconfig.AppConfig{AppID: "web", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: port}}}
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()

	out := captureStderr(t, func() { r.runOne(runCtx, runCtx, cfg) })
	if heads() == 0 {
		t.Fatalf("readiness did not reach the app through the tunnel:\n%s", out)
	}
	local := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if body := getBody(t, "http://"+local+"/"); body != "app" {
		t.Fatalf("body through the tunnel = %q", body)
	}

	runCancel()
	r.reap()
	if c, err := net.DialTimeout("tcp", local, time.Second); err == nil {
		c.Close()
		t.Fatalf("%s still accepts connections after the run ended", local)
	}
}

// fakeClock is the time a tunnelReadiness under test reads.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// simulateSlowStart drives attempt the way waitForReadinessProbe does for
// camera-feed-yolo, whose readiness timeoutSeconds is 180 so a two-minute
// start stays in the first, 500 ms-tick phase: an attempt per tick, and the
// next at once when an attempt outlasts its tick. It returns when an attempt
// passes, or false once window has gone by.
func simulateSlowStart(clock *fakeClock, window time.Duration, attempt func(context.Context) error) (time.Duration, bool) {
	start := clock.now()
	for clock.now().Sub(start) < window {
		tick := clock.now().Add(500 * time.Millisecond)
		if attempt(context.Background()) == nil {
			return clock.now().Sub(start), true
		}
		if clock.now().Before(tick) {
			clock.t = tick
		}
	}
	return 0, false
}

// How many Wendy Cloud relays readiness opens while an app takes two minutes
// to listen (camera-feed-yolo loading its model on a Jetson Orin Nano), each
// relay taking a second to set up and fail. Every attempt used to open one.
// Now the agent is asked first, over the existing connection, and a relay
// opens to confirm once it reports the port; an agent that cannot answer gets
// backed-off relays, and one that never reports the port gets the spaced
// safety relays.
func TestTunnelReadinessRelaysOverSlowStart(t *testing.T) {
	const (
		appUp        = 2 * time.Minute
		window       = 180 * time.Second
		relayLatency = time.Second
	)
	type scenario struct {
		clock  *fakeClock
		start  time.Time
		relays int // relays opened
		calls  int // GetContainerPorts calls over the existing connection
	}
	newScenario := func() *scenario {
		start := time.Unix(1_700_000_000, 0)
		return &scenario{clock: &fakeClock{t: start}, start: start}
	}
	relay := func(s *scenario) func(context.Context) error {
		return func(context.Context) error {
			s.relays++
			s.clock.t = s.clock.t.Add(relayLatency)
			if s.clock.t.Sub(s.start) < appUp {
				return errors.New("relay closed: connection refused on the device")
			}
			return nil
		}
	}
	agent := func(s *scenario, reports bool, err error) func(context.Context) (bool, error) {
		return func(context.Context) (bool, error) {
			s.calls++
			if err != nil {
				return false, err
			}
			return reports && s.clock.t.Sub(s.start) >= appUp, nil
		}
	}
	run := func(t *testing.T, s *scenario, attempt func(context.Context) error) time.Duration {
		t.Helper()
		readyAt, ok := simulateSlowStart(s.clock, window, attempt)
		if !ok {
			t.Fatalf("never ready within %s; %d relays", window, s.relays)
		}
		return readyAt
	}

	// Before: each attempt opened its own relay.
	before := newScenario()
	run(t, before, relay(before))
	t.Logf("before: %d relays", before.relays)
	if before.relays < 100 {
		t.Fatalf("the per-attempt baseline opened %d relays; the model no longer matches the old probe", before.relays)
	}

	for _, tc := range []struct {
		name      string
		reports   bool
		agentErr  error
		maxRelays int
		maxLate   time.Duration
	}{
		// The agent lists the app's listening port: relays wait on it.
		{name: "agent reports the port", reports: true, maxRelays: 8, maxLate: 2 * relayLatency},
		// An agent from before GetContainerPorts: backed-off relays.
		{name: "agent without GetContainerPorts", agentErr: status.Error(codes.Unimplemented, "unknown method"), maxRelays: 25, maxLate: tunnelRelayBackoffMax + relayLatency},
		// An agent that answers but never sees the socket: safety relays.
		{name: "agent never reports the port", reports: false, maxRelays: 8, maxLate: tunnelSafetyRelayMax + relayLatency},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScenario()
			var replies []bool
			r := newTunnelReadiness(s.clock.now, agent(s, tc.reports, tc.agentErr), func(ctx context.Context, reply bool) error {
				replies = append(replies, reply)
				return relay(s)(ctx)
			})
			readyAt := run(t, s, r.attempt)
			t.Logf("after: %d relays (before %d), %d agent calls, ready at %s", s.relays, before.relays, s.calls, readyAt)
			if s.relays > tc.maxRelays {
				t.Fatalf("opened %d relays over a %s start, want at most %d", s.relays, appUp, tc.maxRelays)
			}
			if late := readyAt - appUp; late > tc.maxLate {
				t.Fatalf("ready %s after the app answered, want within %s", late, tc.maxLate)
			}
			if tc.agentErr != nil && s.calls != 1 {
				t.Fatalf("asked an agent without GetContainerPorts %d times, want once", s.calls)
			}
			if tc.agentErr == nil && s.calls > int(appUp/devicePortsPollInterval)+30 {
				t.Fatalf("asked the agent %d times, want at most one call per %s", s.calls, devicePortsPollInterval)
			}
			// A relay needs the app to answer unless the agent has seen the
			// socket.
			if last := replies[len(replies)-1]; last != !tc.reports {
				t.Fatalf("the passing relay asked for a reply = %v, want %v", last, !tc.reports)
			}
		})
	}
}

// The relay backoff only grows: 1 s after the first relay, then 2, 4 and 5 s,
// and a failure never shortens it.
func TestTunnelReadinessBackoffGrowsToCap(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	var at []time.Duration
	start := clock.now()
	r := newTunnelReadiness(clock.now, nil, func(context.Context, bool) error {
		at = append(at, clock.now().Sub(start))
		return errors.New("no reply")
	})
	for clock.now().Sub(start) < 30*time.Second {
		_ = r.attempt(context.Background())
		clock.t = clock.t.Add(100 * time.Millisecond)
	}
	want := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second, 12 * time.Second, 17 * time.Second, 22 * time.Second, 27 * time.Second}
	if fmt.Sprint(at) != fmt.Sprint(want) {
		t.Fatalf("relays at %v, want %v", at, want)
	}
}

// trackedRelay is one fake relay whose app reads the request and answers
// when answer closes. Close ends the app's side too.
type trackedRelay struct {
	net.Conn
	once    sync.Once
	closed  chan struct{}
	onClose func()
}

func (c *trackedRelay) Close() error {
	c.once.Do(func() {
		close(c.closed)
		c.onClose()
	})
	return c.Conn.Close()
}

// A readiness probe opens its relay itself and closes it before it returns,
// answered or timed out, and a second probe on the same tunnel does not open
// another while the first holds one.
func TestCloudAppTunnelProbeRelayHoldsOneRelayAndClosesIt(t *testing.T) {
	var mu sync.Mutex
	open, maxOpen, opened := 0, 0, 0
	answer := make(chan struct{})
	requested := make(chan struct{}, 4)
	tunnel := &cloudAppTunnel{LocalPort: 3005, RemotePort: 3005, relay: func(ctx context.Context) (net.Conn, error) {
		client, app := net.Pipe()
		mu.Lock()
		open++
		opened++
		maxOpen = max(maxOpen, open)
		answer := answer
		mu.Unlock()
		relay := &trackedRelay{Conn: client, closed: make(chan struct{}), onClose: func() {
			mu.Lock()
			open--
			mu.Unlock()
		}}
		go func() {
			defer app.Close()
			buf := make([]byte, 512)
			if _, err := app.Read(buf); err != nil {
				return
			}
			requested <- struct{}{}
			select {
			case <-answer:
				_, _ = app.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
			case <-relay.closed:
			}
		}()
		return relay, nil
	}}
	counts := func() (int, int, int) {
		mu.Lock()
		defer mu.Unlock()
		return open, maxOpen, opened
	}

	first := make(chan error, 1)
	go func() { first <- tunnel.probeRelay(context.Background(), true) }()
	<-requested
	if err := tunnel.probeRelay(context.Background(), true); err == nil {
		t.Fatal("a second probe passed while the first held the tunnel's probe relay")
	}
	if _, _, opened := counts(); opened != 1 {
		t.Fatalf("opened %d relays with one probe in flight, want 1", opened)
	}
	close(answer)
	if err := <-first; err != nil {
		t.Fatalf("probe with a reply failed: %v", err)
	}
	if open, _, _ := counts(); open != 0 {
		t.Fatalf("%d relays still open after the probe returned", open)
	}

	// An app that never answers: the attempt's deadline closes the relay.
	mu.Lock()
	answer = make(chan struct{})
	mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	if err := tunnel.probeRelay(ctx, true); err == nil {
		t.Fatal("probe passed with no reply")
	}
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Fatalf("timed-out probe took %s to return", elapsed)
	}
	if open, maxOpen, opened := counts(); open != 0 || maxOpen != 1 || opened != 2 {
		t.Fatalf("open=%d maxOpen=%d opened=%d, want 0, 1, 2", open, maxOpen, opened)
	}
}

// End to end on a real tunnel: while the agent reports the app is not
// listening yet, readiness opens no relay; once it reports the forwarded
// port, one relay confirms Wendy Cloud forwards it, with no request to the
// app.
func TestWaitForTunneledReadinessWaitsOnAgentThenConfirmsOnce(t *testing.T) {
	appPort, heads := headServer(t)
	port := freeLoopbackPort(t)
	var mu sync.Mutex
	var dialed []int
	conn := offNetworkCloudConn(t)
	conn.RegistryDialer = appBehindCloud(net.JoinHostPort("127.0.0.1", strconv.Itoa(appPort)), &dialed, &mu)
	agent := conn.ContainerService.(*tunnelFakeContainerClient)
	var asked []string
	agent.ports = func(req *agentpb.GetContainerPortsRequest) []*agentpb.PortEntry {
		asked = append(asked, req.GetAppName())
		if len(asked) <= 2 {
			// Loading the model: only a UDP socket so far.
			return []*agentpb.PortEntry{{Protocol: "udp", Port: uint32(port), Address: "0.0.0.0"}}
		}
		return []*agentpb.PortEntry{{Protocol: "tcp6", Port: uint32(port), Address: "::"}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tunnel, err := openCloudAppTunnel(ctx, conn, port)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.Close()
	cfg := &appconfig.AppConfig{
		AppID:     "camera-feed-yolo",
		Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: port}, TimeoutSeconds: 10},
	}

	out := captureStderr(t, func() {
		if err := waitForTunneledReadiness(ctx, conn, cfg, tunnel, 0); err != nil {
			t.Errorf("readiness: %v", err)
		}
	})
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) != 1 || dialed[0] != port {
		t.Fatalf("relays opened to %v, want one to port %d after the agent reported it", dialed, port)
	}
	if heads() != 0 {
		t.Fatalf("app got %d HEAD requests, want none once the agent saw its socket", heads())
	}
	if agent.portCalls() != 3 || asked[0] != "camera-feed-yolo" {
		t.Fatalf("asked the agent %d times for %v, want 3 for camera-feed-yolo", agent.portCalls(), asked)
	}
	if !strings.Contains(out, fmt.Sprintf("Waiting for localhost:%d to be ready", port)) || !strings.Contains(out, "Ready.") {
		t.Fatalf("readiness output changed:\n%s", out)
	}
}

// An app that does not speak HTTP (an MQTT broker, say) closes a relay that
// sends it a request without answering. Once the agent reports its port, it
// is ready as a TCP readiness check on the device's network would find it.
func TestWaitForTunneledReadinessPassesAppThatDoesNotAnswerHTTP(t *testing.T) {
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	go func() {
		for {
			c, err := app.Accept()
			if err != nil {
				return
			}
			go func() {
				// Read whatever arrives, never answer, close on input.
				defer c.Close()
				_, _ = c.Read(make([]byte, 512))
			}()
		}
	}()
	port := freeLoopbackPort(t)
	var mu sync.Mutex
	var dialed []int
	conn := offNetworkCloudConn(t)
	conn.RegistryDialer = appBehindCloud(app.Addr().String(), &dialed, &mu)
	conn.ContainerService.(*tunnelFakeContainerClient).ports = func(*agentpb.GetContainerPortsRequest) []*agentpb.PortEntry {
		return []*agentpb.PortEntry{{Protocol: "tcp", Port: uint32(port), Address: "0.0.0.0"}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tunnel, err := openCloudAppTunnel(ctx, conn, port)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.Close()
	cfg := &appconfig.AppConfig{
		AppID:     "mqtt-broker",
		Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: port}, TimeoutSeconds: 5},
	}

	start := time.Now()
	captureStderr(t, func() {
		if err := waitForTunneledReadiness(ctx, conn, cfg, tunnel, 0); err != nil {
			t.Errorf("readiness: %v", err)
		}
	})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("ready after %s, want the first attempt", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) != 1 {
		t.Fatalf("opened %d relays, want 1", len(dialed))
	}
}

// Only a relay Cloud would not authorize, or a service it does not offer,
// fails the same way every time. Transport trouble and an app that is not
// up yet are retried.
func TestPermanentRelayError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"authorization denied", fmt.Errorf("authorizing Cloud tunnel (RequestTunnel): %w", status.Error(codes.PermissionDenied, "no tunnel access")), true},
		{"signed out", status.Error(codes.Unauthenticated, "token expired"), true},
		{"port tunnels not offered", fmt.Errorf("Cloud does not offer tunnel service %q: %w", "tcp-3005", status.Error(codes.NotFound, "unknown service")), true},
		{"Cloud unavailable", fmt.Errorf("joining Cloud relay: %w", status.Error(codes.Unavailable, "connection reset")), false},
		{"relay deadline", status.Error(codes.DeadlineExceeded, "context deadline exceeded"), false},
		{"device closed the relay", io.EOF, false},
		{"canceled", context.Canceled, false},
		{"plain error", errors.New("dial tcp: connection refused"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := permanentRelayError(tc.err); got != tc.want {
				t.Fatalf("permanentRelayError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// When Wendy Cloud refuses the readiness relay, the wait stops at once
// instead of retrying through the timeout and the slow-start observation:
// the tunnel closes and the host-side lifecycle is skipped with the reason,
// not reported as an app that is not ready.
func TestCloudRelayRefusedSkipsHostLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, conn *grpcclient.AgentConnection, cfg *appconfig.AppConfig, port int)
	}{
		{"attached run", func(t *testing.T, conn *grpcclient.AgentConnection, cfg *appconfig.AppConfig, _ int) {
			if cmd := runPostStartIfReady(context.Background(), context.Background(), conn, cfg, runOptions{}); cmd != nil {
				t.Errorf("started a hook after Cloud refused the relay: %v", cmd)
			}
		}},
		{"service hook runner", func(t *testing.T, conn *grpcclient.AgentConnection, cfg *appconfig.AppConfig, port int) {
			r := &serviceHookRunner{conn: conn}
			r.runOne(context.Background(), context.Background(), cfg)
			// Closed by the refusal, before reap closes the run's tunnels.
			if c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second); err == nil {
				c.Close()
				t.Errorf("localhost:%d still accepts connections after Cloud refused the relay", port)
			}
			r.reap()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opened := swapBrowserOpen(t)
			execs := swapPostStartExec(t)
			port := freeLoopbackPort(t)
			var mu sync.Mutex
			relays := 0
			conn := offNetworkCloudConn(t)
			conn.RegistryDialer = func(context.Context, int) (net.Conn, error) {
				mu.Lock()
				relays++
				mu.Unlock()
				return nil, fmt.Errorf("authorizing Cloud tunnel (RequestTunnel): %w", status.Error(codes.PermissionDenied, "no tunnel access for this device"))
			}
			cfg := &appconfig.AppConfig{
				AppID:     "camera-feed-yolo",
				Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: port}, TimeoutSeconds: 5},
				Hooks: &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{
					OpenURL: fmt.Sprintf("http://${WENDY_HOSTNAME}:%d", port),
					CLI:     fmt.Sprintf("echo http://${WENDY_HOSTNAME}:%d", port),
				}},
			}

			start := time.Now()
			out := captureStderr(t, func() { tc.run(t, conn, cfg, port) })
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Errorf("took %s; a refused relay must end the readiness wait", elapsed)
			}
			mu.Lock()
			if relays != 1 {
				t.Errorf("opened %d relays, want 1", relays)
			}
			mu.Unlock()
			if len(*opened) != 0 || len(*execs) != 0 {
				t.Fatalf("ran hooks after Cloud refused the relay: browser=%v cli=%v", *opened, *execs)
			}
			if c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second); err == nil {
				c.Close()
				t.Errorf("localhost:%d still accepts connections after Cloud refused the relay", port)
			}
			for _, line := range []string{
				fmt.Sprintf("Could not forward port %d to camera-feed-yolo: Wendy Cloud refused the relay", port),
				"no tunnel access for this device",
				"Connect the desktop VPN to reach this cloud device.",
			} {
				if !strings.Contains(out, line) {
					t.Errorf("missing %q in output:\n%s", line, out)
				}
			}
			for _, line := range []string{"Warning:", "is not ready", "still starting", "Wendy Cloud tunnel to port"} {
				if strings.Contains(out, line) {
					t.Errorf("unexpected %q in output:\n%s", line, out)
				}
			}
		})
	}
}

// A relay that fails for a reason that can pass (Cloud briefly unavailable)
// is reported once and retried on the backoff.
func TestWaitForTunneledReadinessRetriesTransientRelayFailure(t *testing.T) {
	appPort, heads := headServer(t)
	port := freeLoopbackPort(t)
	var mu sync.Mutex
	relays := 0
	conn := offNetworkCloudConn(t)
	conn.RegistryDialer = func(ctx context.Context, _ int) (net.Conn, error) {
		mu.Lock()
		relays++
		first := relays == 1
		mu.Unlock()
		if first {
			return nil, fmt.Errorf("joining Cloud relay: %w", status.Error(codes.Unavailable, "connection reset"))
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(appPort)))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tunnel, err := openCloudAppTunnel(ctx, conn, port)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.Close()
	cfg := &appconfig.AppConfig{
		AppID:     "web",
		Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: port}, TimeoutSeconds: 10},
	}

	out := captureStderr(t, func() {
		if err := waitForTunneledReadiness(ctx, conn, cfg, tunnel, 0); err != nil {
			t.Errorf("readiness: %v", err)
		}
	})
	mu.Lock()
	defer mu.Unlock()
	if relays != 2 || heads() != 1 {
		t.Fatalf("relays=%d HEADs=%d, want 2 and 1", relays, heads())
	}
	if !strings.Contains(out, fmt.Sprintf("Wendy Cloud tunnel to port %d on Dev Kit failed", port)) || !strings.Contains(out, "Ready.") {
		t.Fatalf("output:\n%s", out)
	}
}
