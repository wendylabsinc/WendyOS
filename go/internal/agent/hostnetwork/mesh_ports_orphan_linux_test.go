//go:build linux

package hostnetwork

import (
	"os"
	"strings"
	"testing"
)

func TestOrphanPortProtocolMigrationNamespace(t *testing.T) {
	if os.Getenv("WENDY_TEST_NETNS") != "1" {
		t.Skip("requires disposable privileged network namespace")
	}
	if err := InitMeshPortsChain(); err != nil {
		t.Fatal(err)
	}
	const port = 47777
	if err := AddIngressPortForward(port, "10.201.0.2", 7777); err != nil {
		t.Fatal(err)
	}
	if err := AddIngressUDPPortForward(port, "10.201.0.3", 7777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = RemoveIngressUDPPortForward(port, "10.201.0.3", 7777) })
	if err := FlushOrphanMeshPort(port, "tcp"); err != nil {
		t.Fatal(err)
	}
	forward, err := meshPortsIPTables("-t", "nat", "-S", MeshPortsChainName)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(forward), "-p tcp ") || !strings.Contains(string(forward), "-p udp ") {
		t.Fatalf("wrong protocol survived migration: %s", forward)
	}
	post, err := meshPortsIPTables("-t", "nat", "-S", "POSTROUTING")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(post), "-d 10.201.0.2/32 -p tcp ") || !strings.Contains(string(post), "-d 10.201.0.3/32 -p udp ") {
		t.Fatalf("wrong hairpin survived migration: %s", post)
	}
}

func TestUDPInterceptionServiceOwnersNamespace(t *testing.T) {
	if os.Getenv("WENDY_TEST_NETNS") != "1" {
		t.Skip("requires disposable privileged network namespace")
	}
	if err := InitMeshUDPTProxy(50059); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = CloseMeshUDPTProxy() })
	add := func(ip, owner string) {
		t.Helper()
		if err := AddMeshUDPIntercept(ip, "10.99.0.0/16", owner, 50059); err != nil {
			t.Fatal(err)
		}
	}
	// Services share an app bridge, but have independent container IDs and IPs.
	add("10.201.0.2", "app.service-one")
	add("10.201.0.3", "app.service-two")
	add("10.201.0.4", "app.service-one")
	// A delayed cleanup of the old incarnation must preserve both live rules.
	if err := RemoveMeshUDPIntercept("10.201.0.2", "10.99.0.0/16", "app.service-one", 50059); err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]bool{"10.201.0.2": false, "10.201.0.3": true, "10.201.0.4": true} {
		got, err := meshUDPRuleExists(MeshUDPChainName, meshUDPInterceptArgs(ip, "10.99.0.0/16", 50059))
		if err != nil || got != want {
			t.Fatalf("rule for %s=%v want %v: %v", ip, got, want, err)
		}
	}
}
