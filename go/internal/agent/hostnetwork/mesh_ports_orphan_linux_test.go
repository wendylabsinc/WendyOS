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
