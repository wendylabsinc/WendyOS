package discovery

import (
	"net"
	"testing"
)

func TestUSBDirectCandidateHostPort(t *testing.T) {
	c := USBDirectCandidate{Interface: "enxaabbccddeeff", Zone: "enxaabbccddeeff"}
	got := c.HostPort(50051)
	want := "[fe80::5741:1%enxaabbccddeeff]:50051"
	if got != want {
		t.Fatalf("HostPort = %q, want %q", got, want)
	}
}

func TestUSBDirectCandidatesFrom(t *testing.T) {
	stubBridgeZone(t, nil)
	ifaces := []net.Interface{
		{Index: 3, Name: "enxaabbccddeeff", Flags: net.FlagUp},        // USB by name → candidate
		{Index: 4, Name: "lo0", Flags: net.FlagUp | net.FlagLoopback}, // loopback → skipped
		{Index: 5, Name: "wlan1", Flags: net.FlagUp},                  // not USB → skipped
		{Index: 6, Name: "usb0", Flags: 0},                            // USB name but DOWN → skipped
		{Index: 7, Name: "ncm0", Flags: net.FlagUp},                   // NCM adapter → candidate
	}

	got := usbDirectCandidatesFrom(ifaces, "linux", nil)
	want := []USBDirectCandidate{
		{Interface: "enxaabbccddeeff", Zone: "enxaabbccddeeff"},
		{Interface: "ncm0", Zone: "ncm0"},
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Windows zones are numeric interface indexes, not names.
	gotWin := usbDirectCandidatesFrom(ifaces, "windows", nil)
	if len(gotWin) != 2 || gotWin[0].Zone != "3" || gotWin[1].Zone != "7" {
		t.Fatalf("windows candidates = %+v, want zones \"3\" and \"7\"", gotWin)
	}
}

// On macOS a gadget link is a bare BSD name that matches no name heuristic —
// only the "Hardware Port" display name identifies it. Without the resolver the
// whole feature is inert there.
func TestUSBDirectCandidatesFromDarwinDisplayNames(t *testing.T) {
	stubBridgeZone(t, nil)
	ifaces := []net.Interface{
		{Index: 4, Name: "en0", Flags: net.FlagUp}, // Wi-Fi → skipped
		{Index: 5, Name: "en5", Flags: net.FlagUp}, // gadget NCM → candidate
		{Index: 6, Name: "en7", Flags: net.FlagUp}, // Thunderbolt bridge → skipped
	}
	displayNames := map[string]string{
		"en0": "Wi-Fi",
		"en5": "Wendy USB NCM",
		"en7": "Thunderbolt Bridge",
	}
	var asked []string
	resolve := func(name string) string {
		asked = append(asked, name)
		return displayNames[name]
	}

	got := usbDirectCandidatesFrom(ifaces, "darwin", resolve)
	if len(got) != 1 || got[0] != (USBDirectCandidate{Interface: "en5", Zone: "en5"}) {
		t.Fatalf("candidates = %+v, want just en5", got)
	}
	// Darwin zones are BSD names, and every interface needed the resolver
	// because none of them match on name alone.
	if len(asked) != 3 {
		t.Fatalf("resolver asked for %v, want all three interfaces", asked)
	}
}

// A Windows friendly name ("Ethernet 3") is equally opaque; the adapter
// description carries the USB signal, while the zone stays the numeric index.
func TestUSBDirectCandidatesFromWindowsDescriptions(t *testing.T) {
	ifaces := []net.Interface{
		{Index: 11, Name: "Ethernet", Flags: net.FlagUp},
		{Index: 12, Name: "Ethernet 3", Flags: net.FlagUp},
	}
	descriptions := map[string]string{
		"Ethernet":   "Intel(R) Ethernet Connection I219-V",
		"Ethernet 3": "Wendy USB NCM Network Adapter",
	}

	got := usbDirectCandidatesFrom(ifaces, "windows", func(n string) string { return descriptions[n] })
	if len(got) != 1 || got[0] != (USBDirectCandidate{Interface: "Ethernet 3", Zone: "12"}) {
		t.Fatalf("candidates = %+v, want Ethernet 3 with zone \"12\"", got)
	}
}

// The resolver stands behind a system query, so interfaces already classified
// by name must not trigger it.
func TestUSBDirectCandidatesFromSkipsResolverWhenNameSuffices(t *testing.T) {
	stubBridgeZone(t, nil)
	ifaces := []net.Interface{
		{Index: 3, Name: "enxaabbccddeeff", Flags: net.FlagUp},
		{Index: 4, Name: "lo0", Flags: net.FlagUp | net.FlagLoopback},
		{Index: 5, Name: "usb0", Flags: 0}, // down → never reaches the resolver
	}

	got := usbDirectCandidatesFrom(ifaces, "linux", func(name string) string {
		t.Fatalf("resolver must not be consulted for %q", name)
		return ""
	})
	if len(got) != 1 || got[0].Interface != "enxaabbccddeeff" {
		t.Fatalf("candidates = %+v, want just enxaabbccddeeff", got)
	}
}
