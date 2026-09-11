package commands

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestVMAppPortsIncludesEveryHTTPAndReadinessPort(t *testing.T) {
	cfg := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: "http", Port: 8080}, {Type: "http", Port: 9000}}, Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 8081}}}
	if got := vmAppPorts(nil, cfg, cfg); !reflect.DeepEqual(got, []int{8080, 8081, 9000}) {
		t.Fatal(got)
	}
}

func TestVMAppPortsIncludesPublishedNetworkPortsWithoutReadiness(t *testing.T) {
	web := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork,
		Ports: []appconfig.PortMapping{{Host: 18082, Container: 80}, {Host: 18443, Container: 443}}}}}
	other := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork,
		Ports: []appconfig.PortMapping{{Host: 18083, Container: 80}, {Host: 18082, Container: 80}}}}}
	if got := vmAppPorts(web, nil, other); !reflect.DeepEqual(got, []int{18082, 18083, 18443}) {
		t.Fatalf("published ports = %v", got)
	}
}

func TestVMAppPortsMatchFullEndpointAndCorrectURL(t *testing.T) {
	oldStatuses, oldForward := vmStatusesFn, forwardVMPorts
	t.Cleanup(func() { vmStatusesFn = oldStatuses; forwardVMPorts = oldForward })
	vmStatusesFn = func() ([]vm.Status, error) {
		return []vm.Status{
			{Name: "first", Running: true, State: vm.State{NetMode: vm.NetUser, AgentPort: 50051}},
			{Name: "second", Running: true, State: vm.State{NetMode: vm.NetUser, AgentPort: 50053}},
		}, nil
	}
	var names []string
	forwardVMPorts = func(_ context.Context, name string, ports []int) error {
		names = append(names, name)
		if !reflect.DeepEqual(ports, []int{18080}) {
			t.Fatal(ports)
		}
		return nil
	}
	cfg := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: "http", Port: 18080}}}
	conn := &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: "127.0.0.1:50054", AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}}}
	if err := prepareVMAppPorts(context.Background(), conn, cfg); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"second"}) {
		t.Fatal(names)
	}
	if host, ok := resolveHookHost(context.Background(), conn, cfg); !ok || host != "127.0.0.1" {
		t.Fatalf("%q %v", host, ok)
	}
	if ip := announceReachableURL(context.Background(), conn, cfg); ip != "127.0.0.1" {
		t.Fatalf("announced IP %q", ip)
	}
	conn.Addr = "127.0.0.1:50100"
	if err := prepareVMAppPorts(context.Background(), conn, cfg); err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 {
		t.Fatal("forwarded an unrelated loopback agent")
	}
	conn.Addr = "127.0.0.1:50054"
	forwardVMPorts = func(context.Context, string, []int) error { return fmt.Errorf("port occupied") }
	if err := prepareVMAppPorts(context.Background(), conn, cfg); err == nil {
		t.Fatal("suppressed port conflict")
	}
}

// Opt-in live regression. Start an HTTP server returning Wendy-HTTP-review on
// guest port 18080, then set WENDY_VM_HTTP_TEST to its local VM name and
// WENDY_VM_HTTP_ROOT to the absolute VM store path. This tests
// the same port preparation and readiness/URL paths that run invokes, without
// provisioning a disposable guest or changing the developer's device pins.
func TestVMHTTPIntegration(t *testing.T) {
	name := os.Getenv("WENDY_VM_HTTP_TEST")
	if name == "" {
		t.Skip("requires an explicit test VM")
	}
	root := os.Getenv("WENDY_VM_HTTP_ROOT")
	if root == "" {
		t.Fatal("WENDY_VM_HTTP_ROOT must explicitly select the live VM store")
	}
	s := &vm.Store{Root: root}
	oldStatuses, oldForward := vmStatusesFn, forwardVMPorts
	t.Cleanup(func() { vmStatusesFn, forwardVMPorts = oldStatuses, oldForward })
	vmStatusesFn = s.Statuses
	forwardVMPorts = s.EnsureTCPPorts
	st, err := s.Status(name)
	if err != nil || !st.Running {
		t.Fatalf("VM not running: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := grpcclient.Connect(ctx, fmt.Sprintf("127.0.0.1:%d", st.State.AgentPort))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cfg := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: "http", Port: 18080}}}
	for range 2 {
		if err := prepareVMAppPorts(ctx, conn, cfg); err != nil {
			t.Fatal(err)
		}
	}
	host, ok := resolveHookHost(ctx, conn, cfg)
	if !ok || host != "127.0.0.1" {
		t.Fatalf("host %q, ok %v", host, ok)
	}
	if err := waitForReadiness(ctx, effectiveReadiness(cfg), host); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://" + host + ":18080/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Wendy-HTTP-review") {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	t.Logf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
