package containerd

import (
	"net"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestDeclaredHostPorts(t *testing.T) {
	host := appconfig.Entitlement{Type: appconfig.EntitlementNetwork, Mode: "host"}
	http := appconfig.Entitlement{Type: appconfig.EntitlementHTTP, Port: 8000}
	mcp := appconfig.Entitlement{Type: appconfig.EntitlementMCP, Port: 9000}
	if got := declaredHostPorts([]appconfig.Entitlement{host, http, mcp}); len(got) != 2 || got[0] != 8000 || got[1] != 9000 {
		t.Fatalf("host-network ports = %v, want [8000 9000]", got)
	}
	// Without the host network the port lives in the app's own namespace and cannot clash.
	mesh := appconfig.Entitlement{Type: appconfig.EntitlementNetwork, Mode: "mesh"}
	for _, ents := range [][]appconfig.Entitlement{{http}, {mesh, http}} {
		if got := declaredHostPorts(ents); got != nil {
			t.Fatalf("ports for %v = %v, want none", ents, got)
		}
	}
}

func TestHostPortBusy(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if !hostPortBusy(port) {
		t.Fatalf("port %d reported free while a listener holds it", port)
	}
	_ = l.Close()
	if hostPortBusy(port) {
		t.Fatalf("port %d reported busy after the listener closed", port)
	}
}

func TestCheckHostPortsPassesFreeAndUndeclaredPorts(t *testing.T) {
	orig := hostPortBusy
	t.Cleanup(func() { hostPortBusy = orig })
	hostPortBusy = func(int) bool { return false }
	c := &Client{}
	ents := []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "host"}, {Type: appconfig.EntitlementHTTP, Port: 8000}}
	if err := c.checkHostPorts(t.Context(), "app", ents); err != nil {
		t.Fatalf("free port refused: %v", err)
	}
	hostPortBusy = func(int) bool { t.Fatal("probed a port for an app that declares none"); return true }
	if err := c.checkHostPorts(t.Context(), "app", []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "host"}}); err != nil {
		t.Fatal(err)
	}
}
