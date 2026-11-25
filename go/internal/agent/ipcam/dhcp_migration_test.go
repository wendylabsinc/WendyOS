package ipcam

import (
	"reflect"
	"testing"
)

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
			tc.cam.MAC = "02:00:00:00:00:01"
			credentials := &CredentialStore{by: map[string]Credential{tc.cam.MAC: {Username: "admin"}}}
			got := legacyDHCPInterfaces([]Camera{tc.cam, tc.cam}, credentials)
			if got[tc.cam.Link] != tc.want || (!tc.want && len(got) != 0) || len(got) > 1 {
				t.Fatalf("legacy interfaces = %v, want inclusion %v", got, tc.want)
			}
		})
	}
}

// A saved lease, served flag, hostname or discovery metadata must never let
// credentials for a different device authorize this port.
func TestLegacyDHCPInterfacesRequiresMatchingCredentials(t *testing.T) {
	cameras := []Camera{
		{MAC: "02:00:00:00:00:01", Link: "eth0", Address: "10.98.0.50", Model: "OfficePrinter", DHCPServed: true},
		{MAC: "02:00:00:00:00:02", Link: "eth1", Address: "10.98.1.149"},
		{MAC: "02:00:00:00:00:03", Link: "eth2", Address: "10.98.2.50", ONVIFAddr: "http://camera/onvif", StreamMain: "/main"},
		{Link: "eth3", Address: "10.98.3.50"},
	}
	credentials := &CredentialStore{by: map[string]Credential{
		"02:00:00:00:00:02": {Username: "admin"},
		"02:00:00:00:00:99": {Username: "other"},
		"":                  {Username: "invalid"},
	}}
	if got := legacyDHCPInterfaces(cameras, credentials); !reflect.DeepEqual(got, map[string]bool{"eth1": true}) {
		t.Fatalf("migrated interfaces = %v, want only MAC-matched eth1", got)
	}
	if got := legacyDHCPInterfaces(cameras, nil); len(got) != 0 {
		t.Fatalf("migrated without credentials: %v", got)
	}
}
