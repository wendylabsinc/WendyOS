//go:build linux

package localmesh

import (
	"github.com/hashicorp/mdns"
	"github.com/miekg/dns"
	"net"
	"strings"
	"testing"
)

func TestTLSLANDiscoveryHasIndependentServiceAndCapability(t *testing.T) {
	self, peer := lanDevice(t, 445), lanDevice(t, 460)
	selected := lanInterface{iface: net.Interface{Index: 3}, ip: net.IPv4(192, 168, 2, 3)}
	for _, role := range []string{"router", "embedded-router"} {
		zone, err := tlsLANMDNSService(&Credentials{Device: peer}, selected, role)
		if err != nil {
			t.Fatal(err)
		}
		m := &dns.Msg{Answer: zone.Records(dns.Question{Name: tlsLANService + ".local.", Qtype: dns.TypePTR, Qclass: dns.ClassINET})}
		wire, err := m.Pack()
		if err != nil {
			t.Fatal(err)
		}
		var received dns.Msg
		if err = received.Unpack(wire); err != nil {
			t.Fatal(err)
		}
		entries := entriesFromLANServiceResponse(&received, tlsLANService)
		if len(entries) != 1 {
			t.Fatal(entries)
		}
		got, ip, kind, ok := parseTLSLANEntry(entries[0], self)
		if !ok || got != peer || kind != role || !ip.Equal(selected.ip) {
			t.Fatal(got, ip, kind, ok)
		}
		if _, _, ok := parseLANEntry(entries[0], self); ok {
			t.Fatal("TLS service entered QUIC path")
		}
		for _, change := range []func(*mdns.ServiceEntry){
			func(e *mdns.ServiceEntry) { e.Port = LANPort },
			func(e *mdns.ServiceEntry) { e.InfoFields = append(e.InfoFields, "PrOtOcOl=tls-v1") },
			func(e *mdns.ServiceEntry) { e.InfoFields = append(e.InfoFields, "Role="+role) },
			func(e *mdns.ServiceEntry) {
				for i, f := range e.InfoFields {
					if strings.HasPrefix(f, "protocol=") {
						e.InfoFields[i] = "protocol=quic-v1"
					}
				}
			},
			func(e *mdns.ServiceEntry) {
				for i, f := range e.InfoFields {
					if strings.HasPrefix(f, "version=") {
						e.InfoFields[i] = "version=2"
					}
				}
			},
		} {
			bad := *entries[0]
			bad.InfoFields = append([]string(nil), bad.InfoFields...)
			change(&bad)
			if _, _, _, ok := parseTLSLANEntry(&bad, self); ok {
				t.Fatal("invalid TLS hint accepted", bad)
			}
		}
	}
}
