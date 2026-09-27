package discovery

import (
	"net"
	"testing"
)

// stubBridgeZone replaces the host probes behind LinkLocalDialZone: bridged
// maps a member port to its bridge, and every port in it lacks an IPv6
// link-local address (which is what bridging does on macOS).
func stubBridgeZone(t *testing.T, bridged map[string]string) {
	t.Helper()
	origLL, origMaster := hasIPv6LinkLocalFn, bridgeMasterFn
	t.Cleanup(func() { hasIPv6LinkLocalFn, bridgeMasterFn = origLL, origMaster })
	hasIPv6LinkLocalFn = func(iface string) bool { _, ok := bridged[iface]; return !ok }
	bridgeMasterFn = func(iface string) string { return bridged[iface] }
}

// Trimmed from a real macOS host with Internet Sharing on for a WendyOS USB
// gadget link.
const ifconfigWithInternetSharing = `en0: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	inet 172.21.65.175 netmask 0xffffe000 broadcast 172.21.95.255
en10: flags=8963<UP,BROADCAST,SMART,RUNNING,PROMISC,SIMPLEX,MULTICAST> mtu 1500
	ether 86:a9:ac:3f:cc:18
	inet 169.254.79.201 netmask 0xffff0000 broadcast 169.254.255.255
bridge100: flags=8a63<UP,BROADCAST,SMART,RUNNING,ALLMULTI,SIMPLEX,MULTICAST> mtu 1500
	inet 192.168.2.1 netmask 0xffffff00 broadcast 192.168.2.255
	inet6 fe80::10a1:daff:fe90:f464%bridge100 prefixlen 64 scopeid 0xf
	Configuration:
		id 0:0:0:0:0:0 priority 0 hellotime 0 fwddelay 0
	member: en10 flags=3<LEARNING,DISCOVER>
	        ifmaxaddr 0 port 14 priority 0 path cost 0
	member: en11 flags=3<LEARNING,DISCOVER>
	        ifmaxaddr 0 port 16 priority 0 path cost 0
utun0: flags=8051<UP,POINTOPOINT,RUNNING,MULTICAST> mtu 1380
`

func TestParseBridgeMembers(t *testing.T) {
	got := parseBridgeMembers(ifconfigWithInternetSharing)
	want := map[string]string{"en10": "bridge100", "en11": "bridge100"}
	if len(got) != len(want) {
		t.Fatalf("members = %v, want %v", got, want)
	}
	for member, bridge := range want {
		if got[member] != bridge {
			t.Fatalf("members[%q] = %q, want %q", member, got[member], bridge)
		}
	}
}

func TestLinkLocalDialZone(t *testing.T) {
	stubBridgeZone(t, map[string]string{"en10": "bridge100", "en12": ""})

	for iface, want := range map[string]string{
		"en10": "bridge100", // bridged member → its bridge
		"en5":  "en5",       // has its own link-local → unchanged
		"en12": "en12",      // no link-local and no bridge → unchanged
		"":     "",
	} {
		if got := LinkLocalDialZone(iface); got != want {
			t.Errorf("LinkLocalDialZone(%q) = %q, want %q", iface, got, want)
		}
	}
}

// With Internet Sharing on, the gadget port is still the candidate (and what
// the user sees), but dialing has to go through the bridge.
func TestUSBDirectCandidatesFromBridgedPort(t *testing.T) {
	stubBridgeZone(t, map[string]string{"en10": "bridge100"})
	ifaces := []net.Interface{{Index: 14, Name: "en10", Flags: net.FlagUp}}

	got := usbDirectCandidatesFrom(ifaces, "darwin", func(string) string { return "WendyOS Device enmax01" })
	want := USBDirectCandidate{Interface: "en10", Zone: "bridge100"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("candidates = %+v, want [%+v]", got, want)
	}
	if hp := got[0].HostPort(50052); hp != "[fe80::5741:1%bridge100]:50052" {
		t.Fatalf("HostPort = %q", hp)
	}
}
