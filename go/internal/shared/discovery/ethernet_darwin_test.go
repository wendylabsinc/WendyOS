//go:build darwin

package discovery

import "testing"

func TestParseDarwinHardwarePortsBuildsDisplayNameMap(t *testing.T) {
	out := `Hardware Port: USB 10/100/1000 LAN
Device: en6
Ethernet Address: aa:bb:cc:dd:ee:ff

Hardware Port: Wi-Fi
Device: en0
Ethernet Address: 11:22:33:44:55:66
`

	ports := parseDarwinHardwarePorts(out)
	names := darwinDisplayNamesByInterface(ports)
	if names["en6"] != "USB 10/100/1000 LAN" {
		t.Fatalf("en6 display name = %q, want USB 10/100/1000 LAN", names["en6"])
	}
	if names["en0"] != "Wi-Fi" {
		t.Fatalf("en0 display name = %q, want Wi-Fi", names["en0"])
	}
}
