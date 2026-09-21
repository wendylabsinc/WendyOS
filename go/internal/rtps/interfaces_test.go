package rtps

import (
	"net"
	"slices"
	"testing"
)

func TestDiscoveryInterfacePolicySeparatesHostAndIsolatedScopes(t *testing.T) {
	up := net.FlagUp | net.FlagMulticast
	ifaces := []InterfaceCandidate{
		{Name: "wlan0", Flags: up, HasIPv4: true},
		{Name: "veth0", Flags: up, HasIPv4: true},
		{Name: "eth0", Flags: up, HasIPv4: true},
		{Name: "lo", Flags: up | net.FlagLoopback, HasIPv4: true},
		{Name: "eth1", Flags: up, HasIPv4: true},
	}
	if got := EligibleInterfaces(ifaces, true); !slices.Equal(got, []string{"eth0", "eth1"}) {
		t.Fatalf("host coverage = %v", got)
	}
	if got := EligibleInterfaces(ifaces, false); !slices.Equal(got, []string{"veth0", "eth0", "eth1"}) {
		t.Fatalf("isolated coverage = %v", got)
	}
}
