package commands

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func TestVMRegistryRoutesEveryBuilderToSelectedVM(t *testing.T) {
	stubVMStatuses(t, runningVM("one", vm.NetUser, 50051), runningVM("two", vm.NetUser, 50053))
	old := vmRegistryPort
	t.Cleanup(func() { vmRegistryPort = old })
	ports := map[string]int{}
	for _, name := range []string{"one", "two"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, name) }))
		t.Cleanup(srv.Close)
		_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
		ports[name], _ = strconv.Atoi(port)
	}
	vmRegistryPort = func(_ context.Context, name string) (int, error) { return ports[name], nil }
	for _, builder := range []string{"swift", "docker", "apple"} {
		for i, name := range []string{"one", "two"} {
			conn := &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: fmt.Sprintf("127.0.0.1:%d", 50051+2*i)}
			var addr string
			var done func()
			var err error
			switch builder {
			case "swift":
				addr, _, done, _, err = resolveRegistryForSwiftAgent(context.Background(), conn, 5000)
			case "docker":
				addr, done, _, err = resolveRegistryForAgent(context.Background(), conn, 5000)
			case "apple":
				addr, done, _, _, err = resolveRegistryForAppleContainer(context.Background(), conn, 5000)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.Get("http://127.0.0.1:" + port + "/v2/")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			done()
			conn.Close()
			if err != nil || string(body) != name {
				t.Fatalf("%s for %s reached %q: %v", builder, name, body, err)
			}
		}
	}
	vmRegistryPort = func(context.Context, string) (int, error) { return 0, fmt.Errorf("monitor unavailable") }
	conn := &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: "127.0.0.1:50053"}
	if _, _, _, _, err := resolveRegistryForSwiftAgent(context.Background(), conn, 5000); err == nil {
		t.Fatal("fell back to another VM's registry")
	}
}

func TestNamedVMRegistryFailsClosedWhenTargetChanges(t *testing.T) {
	for _, scenario := range []string{"missing", "stopped", "moved", "reused", "shared", "invalid"} {
		t.Run(scenario, func(t *testing.T) {
			statuses := []vm.Status{runningVM("one", vm.NetUser, 50051)}
			conn := &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: "127.0.0.1:50053", SimulatorName: "two"}
			switch scenario {
			case "stopped":
				statuses = append(statuses, vm.Status{Name: "two", Exists: true})
			case "moved":
				statuses = append(statuses, runningVM("two", vm.NetUser, 50100))
			case "reused":
				statuses = append(statuses, runningVM("replacement", vm.NetUser, 50053))
			case "shared":
				statuses = append(statuses, runningVM("two", vm.NetShared, 0))
			case "invalid":
				conn.Addr = "localhost:bad"
			}
			stubVMStatuses(t, statuses...)
			old := vmRegistryPort
			t.Cleanup(func() { vmRegistryPort = old })
			vmRegistryPort = func(context.Context, string) (int, error) {
				t.Fatal("attempted registry forwarding for a lost or replaced VM")
				return 0, nil
			}
			if _, _, _, _, err := resolveRegistryForSwiftAgent(context.Background(), conn, 5000); err == nil {
				t.Fatal("Swift fell back to localhost")
			}
			if _, _, _, err := resolveRegistryForAgent(context.Background(), conn, 5000); err == nil {
				t.Fatal("Docker fell back to localhost")
			}
			if _, _, _, _, err := resolveRegistryForAppleContainer(context.Background(), conn, 5000); err == nil {
				t.Fatal("Apple Container fell back to localhost")
			}
		})
	}
}

func TestNamedVMEndpointMatchRetainsIdentityForPlaintextAndTLS(t *testing.T) {
	stubVMStatuses(t, runningVM("one", vm.NetUser, 50051), runningVM("two", vm.NetUser, 50053))
	for _, port := range []int{50053, 50054} {
		conn := &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: fmt.Sprintf("127.0.0.1:%d", port), SimulatorName: "two"}
		name, err := userVMForConnection(conn)
		if err != nil || name != "two" {
			t.Fatalf("named endpoint %d = %q: %v", port, name, err)
		}
	}
	// Native loopback agents still use the ordinary registry path.
	conn := &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: "127.0.0.1:50100"}
	if port, err := registryHostPortForAgent(context.Background(), conn, 5000); err != nil || port != 5000 {
		t.Fatalf("native registry = %d: %v", port, err)
	}
}
