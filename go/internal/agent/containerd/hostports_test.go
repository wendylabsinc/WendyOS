package containerd

import (
	"context"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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

func stubHostPorts(t *testing.T, busy bool, holders ...string) {
	t.Helper()
	origBusy, origHolders := hostPortBusy, hostPortHolders
	t.Cleanup(func() { hostPortBusy, hostPortHolders = origBusy, origHolders })
	hostPortBusy = func(int) bool { return busy }
	hostPortHolders = func(*Client, context.Context, int) map[string]bool {
		m := map[string]bool{}
		for _, h := range holders {
			m[h] = true
		}
		return m
	}
}

var hostWeb = []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "host"}, {Type: appconfig.EntitlementHTTP, Port: 8000}}

func TestCheckHostPortsPassesFreeAndUndeclaredPorts(t *testing.T) {
	stubHostPorts(t, false)
	c := &Client{}
	if err := c.checkHostPorts(t.Context(), "app", hostWeb); err != nil {
		t.Fatalf("free port refused: %v", err)
	}
	hostPortBusy = func(int) bool { t.Fatal("probed a port for an app that declares none"); return true }
	if err := c.checkHostPorts(t.Context(), "app", hostWeb[:1]); err != nil {
		t.Fatal(err)
	}
}

func TestCheckHostPortsRefusesAnotherHolderByName(t *testing.T) {
	c := &Client{}
	stubHostPorts(t, true, "other.app")
	err := c.checkHostPorts(t.Context(), "app", hostWeb)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "port 8000 is already used by app other.app") {
		t.Fatalf("err = %v, want a FailedPrecondition naming other.app", err)
	}
	stubHostPorts(t, true)
	if err := c.checkHostPorts(t.Context(), "app", hostWeb); err == nil || !strings.Contains(err.Error(), "another process on the device") {
		t.Fatalf("err = %v, want a refusal naming no app", err)
	}
}

func TestCheckHostPortsPassesTheAppsOwnListener(t *testing.T) {
	c := &Client{}
	// A redeploy over the running version, even when another app shares the port number.
	stubHostPorts(t, true, "app", "a.first")
	if err := c.checkHostPorts(t.Context(), "app", hostWeb); err != nil {
		t.Fatalf("the app's own listener refused: %v", err)
	}
}
