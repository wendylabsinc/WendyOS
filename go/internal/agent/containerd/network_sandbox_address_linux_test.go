//go:build linux

package containerd

import (
	"net"
	"os"
	"testing"
)

func TestNetworkNamespaceAddressErrorChecksLiveEth0(t *testing.T) {
	if os.Getenv("WENDY_TEST_NETNS") != "1" {
		t.Skip("set WENDY_TEST_NETNS=1 and grant CAP_SYS_ADMIN for live netns test")
	}
	eth0, err := net.InterfaceByName("eth0")
	if err != nil {
		t.Skip("test host has no eth0")
	}
	addrs, err := eth0.Addrs()
	if err != nil {
		t.Fatal(err)
	}
	var ip net.IP
	for _, addr := range addrs {
		if parsed, _, parseErr := net.ParseCIDR(addr.String()); parseErr == nil && parsed.To4() != nil {
			ip = parsed
			break
		}
	}
	if ip == nil {
		t.Skip("eth0 has no IPv4 address")
	}
	if !networkNamespaceHasIP("/proc/self/ns/net", ip.String()) {
		t.Fatal("live address rejected")
	}
	if networkNamespaceHasIP("/proc/self/ns/net", "203.0.113.254") {
		t.Fatal("address absent from eth0 was accepted")
	}
}
