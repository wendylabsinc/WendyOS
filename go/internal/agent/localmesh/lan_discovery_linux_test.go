//go:build linux

package localmesh

import (
	"net"
	"strings"
	"testing"

	"github.com/hashicorp/mdns"
	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestLANDiscoveryVersionCompatibility(t *testing.T) {
	self, peer := lanDevice(t, 445), lanDevice(t, 460)
	base := []string{"principal=" + peer.Principal(), "mesh=" + lanMeshHash(self.Tenant()), "protocol=" + lanProtocol}
	for _, tt := range []struct {
		name  string
		extra []string
		want  bool
	}{
		{"versionless native UDP", nil, true},
		{"v1", []string{"version=1"}, true},
		{"v1 with extension", []string{"version=1", "future=opaque"}, true},
		{"mixed case v1", []string{"VeRsIoN=1"}, true},
		{"uppercase future", []string{"VERSION=2"}, false},
		{"mixed case duplicate", []string{"version=1", "Version=1"}, false},
		{"mixed case conflicting", []string{"version=1", "Version=2"}, false},
		{"mixed case principal duplicate", []string{"PRINCIPAL=" + peer.Principal()}, false},
		{"mixed case mesh duplicate", []string{"Mesh=" + lanMeshHash(self.Tenant())}, false},
		{"empty", []string{"version="}, false},
		{"future", []string{"version=2"}, false},
		{"zero", []string{"version=0"}, false},
		{"noncanonical", []string{"version=01"}, false},
		{"space", []string{"version=1 "}, false},
		{"duplicate", []string{"version=1", "version=1"}, false},
		{"conflicting", []string{"version=1", "version=2"}, false},
		{"malformed", []string{"version"}, false},
		{"missing mesh", []string{"version=1"}, false},
		{"missing protocol", []string{"version=1"}, false},
		{"wrong protocol", []string{"version=1"}, false},
		{"mixed case protocol duplicate", []string{"PrOtOcOl=" + lanProtocol}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			info := append(append([]string(nil), base...), tt.extra...)
			if tt.name == "missing mesh" {
				info = []string{base[0], base[2], "version=1"}
			}
			if tt.name == "missing protocol" {
				info = []string{base[0], base[1], "version=1"}
			}
			if tt.name == "wrong protocol" {
				info = []string{base[0], base[1], "protocol=tcp", "version=1"}
			}
			entry := &mdns.ServiceEntry{Port: LANPort, AddrV4: net.IPv4(192, 168, 2, 3), InfoFields: info}
			got, _, ok := parseLANEntry(entry, self)
			if ok != tt.want || ok && got != peer {
				t.Fatalf("accepted=%v identity=%v, want accepted=%v", ok, got, tt.want)
			}
		})
	}
	entry := &mdns.ServiceEntry{Port: LANPort, AddrV4: net.IPv4(192, 168, 2, 3), InfoFields: []string{
		"PrInCiPaL=" + peer.Principal(), "MESH=" + lanMeshHash(self.Tenant()), "PrOtOcOl=" + lanProtocol, "VERSION=1",
	}}
	if got, _, ok := parseLANEntry(entry, self); !ok || got != peer {
		t.Fatalf("valid case-insensitive TXT keys rejected: %v %v", got, ok)
	}
}

func TestLANDiscoveryPublishedDNSRoundTrip(t *testing.T) {
	self, peer := lanDevice(t, 445), lanDevice(t, 460)
	selected := lanInterface{iface: net.Interface{Index: 3}, ip: net.IPv4(192, 168, 2, 3)}
	zone, err := lanMDNSService(&Credentials{Device: peer}, selected)
	if err != nil {
		t.Fatal(err)
	}
	if lanService != "_wendy-net._udp" {
		t.Fatalf("wrong native UDP discovery service: %s", lanService)
	}
	question := dns.Question{Name: lanService + ".local.", Qtype: dns.TypePTR, Qclass: dns.ClassINET}
	message := &dns.Msg{Answer: zone.Records(question)}
	wire, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	var received dns.Msg
	if err := received.Unpack(wire); err != nil {
		t.Fatal(err)
	}
	entries := entriesFromLANResponse(&received)
	if len(entries) != 1 {
		t.Fatalf("advertisement did not survive DNS encoding: %+v", entries)
	}
	fields := map[string]string{}
	for _, txt := range entries[0].InfoFields {
		key, value, ok := strings.Cut(txt, "=")
		if ok {
			fields[strings.ToLower(key)] = value
		}
	}
	if fields["version"] != "1" || fields["protocol"] != lanProtocol {
		t.Fatalf("native protocol/version missing from DNS TXT: %v", entries[0].InfoFields)
	}
	if got, ip, ok := parseLANEntry(entries[0], self); !ok || got != peer || !ip.Equal(selected.ip) {
		t.Fatalf("published discovery was rejected: %v %v %v", got, ip, ok)
	}
	// Versionless means the same native UDP contract with only the optional
	// discovery version absent. The mandatory protocol must survive.
	versionless := *entries[0]
	versionless.InfoFields = nil
	for _, txt := range entries[0].InfoFields {
		key, _, _ := strings.Cut(txt, "=")
		if !strings.EqualFold(key, "version") {
			versionless.InfoFields = append(versionless.InfoFields, txt)
		}
	}
	if got, _, ok := parseLANEntry(&versionless, self); !ok || got != peer {
		t.Fatal("versionless native UDP contract changed")
	}
	missingProtocol := versionless
	missingProtocol.InfoFields = nil
	for _, txt := range versionless.InfoFields {
		key, _, _ := strings.Cut(txt, "=")
		if !strings.EqualFold(key, "protocol") {
			missingProtocol.InfoFields = append(missingProtocol.InfoFields, txt)
		}
	}
	if _, _, ok := parseLANEntry(&missingProtocol, self); ok {
		t.Fatal("missing native UDP protocol accepted")
	}

}

func TestLANMeshHintUnchangedVector(t *testing.T) {
	tenant, err := meshid.ParseTenant("5b8c6c8e-2f9f-4b4a-9a4f-6e2d8c9a0b12")
	if err != nil {
		t.Fatal(err)
	}
	if got := lanMeshHash(tenant); got != "8e6d83e9d13ffe98" {
		t.Fatalf("LAN wire hint changed: %s", got)
	}
}
