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

func TestLinkSpeedFromIfconfig(t *testing.T) {
	tests := []struct {
		media string
		want  string
	}{
		// A WendyOS board's USB network link, as macOS 26 prints it.
		{"media: autoselect (2500Base-T <full-duplex>)", "2.5 Gbps"},
		{"media: autoselect (5000Base-T <full-duplex>)", "5 Gbps"},
		{"media: autoselect (1000baseT <full-duplex>)", "1 Gbps"},
		{"media: autoselect (10GBase-T <full-duplex>)", "10 Gbps"},
		{"media: 10Gbase-T <full-duplex>", "10 Gbps"},
		{"media: autoselect (2.5GBase-T <full-duplex>)", "2.5 Gbps"},
		{"media: 100baseTX <full-duplex>", "100 Mbps"},
		{"media: autoselect", ""},
		{"media: none", ""},
	}
	for _, tt := range tests {
		out := "en11: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500\n" +
			"\tether aa:bb:cc:dd:ee:ff\n" +
			"\t" + tt.media + "\n" +
			"\tstatus: active\n"
		if got := linkSpeedFromIfconfig(out); got != tt.want {
			t.Errorf("linkSpeedFromIfconfig(%q) = %q, want %q", tt.media, got, tt.want)
		}
	}
}
