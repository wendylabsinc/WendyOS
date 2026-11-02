package ipcam

import "testing"

func TestLegacyDHCPInterfaces(t *testing.T) {
	for _, tc := range []struct {
		name string
		cam  Camera
		want bool
	}{
		{"old lease without served flag", Camera{Link: "eth0", Address: "10.98.0.50"}, true},
		{"last lease in last subnet", Camera{Link: "enp2s0", Address: "10.98.255.149"}, true},
		{"served lease", Camera{Link: "eth0", Address: "10.98.3.100", DHCPServed: true}, true},
		{"below pool", Camera{Link: "eth0", Address: "10.98.0.49"}, false},
		{"above pool", Camera{Link: "eth0", Address: "10.98.0.150"}, false},
		{"server address", Camera{Link: "eth0", Address: "10.98.0.1"}, false},
		{"different subnet", Camera{Link: "eth0", Address: "10.99.0.50"}, false},
		{"ordinary LAN", Camera{Link: "eth0", Address: "192.168.0.50"}, false},
		{"passive observation", Camera{Link: "eth0"}, false},
		{"served flag alone", Camera{Link: "eth0", DHCPServed: true}, false},
		{"discovered without port", Camera{Address: "10.98.0.50"}, false},
		{"invalid port", Camera{Link: "eth0\neth1", Address: "10.98.0.50"}, false},
		{"invalid address", Camera{Link: "eth0", Address: "invalid"}, false},
		{"IPv6", Camera{Link: "eth0", Address: "fd00::50"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := legacyDHCPInterfaces([]Camera{tc.cam, tc.cam})
			if got[tc.cam.Link] != tc.want || (!tc.want && len(got) != 0) || len(got) > 1 {
				t.Fatalf("legacy interfaces = %v, want inclusion %v", got, tc.want)
			}
		})
	}
}
