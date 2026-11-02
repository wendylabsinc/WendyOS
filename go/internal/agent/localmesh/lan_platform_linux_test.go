//go:build linux

package localmesh

import (
	"github.com/miekg/dns"
	"net"
	"strings"
	"testing"
)

func TestNormalLANPlatformAdvertisements(t *testing.T) {
	self, peer := lanDevice(t, 445), lanDevice(t, 460)
	selected := lanInterface{iface: net.Interface{Index: 3}, ip: net.IPv4(192, 168, 2, 3)}
	udp, err := lanMDNSService(&Credentials{Device: peer}, selected)
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := tlsLANMDNSService(&Credentials{Device: peer}, selected, "router")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		zone    lanMDNSZone
		service string
	}{
		{"UDP", udp, lanService}, {"TCP", tcp, tlsLANService},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := &dns.Msg{Answer: tc.zone.Records(dns.Question{Name: tc.service + ".local.", Qtype: dns.TypePTR, Qclass: dns.ClassINET})}
			wire, err := message.Pack()
			if err != nil {
				t.Fatal(err)
			}
			var received dns.Msg
			if err = received.Unpack(wire); err != nil {
				t.Fatal(err)
			}
			entries := entriesFromLANServiceResponse(&received, tc.service)
			if len(entries) != 1 {
				t.Fatal(entries)
			}
			platform := ""
			for _, field := range entries[0].InfoFields {
				key, value, ok := strings.Cut(field, "=")
				if ok && strings.EqualFold(key, "platform") {
					platform = value
				}
			}
			if platform != "WendyOS" {
				t.Fatalf("platform=%q", platform)
			}
			if tc.name == "UDP" {
				if device, _, ok := parseLANEntry(entries[0], self); !ok || device != peer {
					t.Fatal("native UDP advertisement unavailable")
				}
			} else {
				device, _, role, ok := parseTLSLANEntry(entries[0], self)
				if !ok || device != peer || shouldDialTLSLAN(entries[0], role) {
					t.Fatal("WendyOS TCP discovery planning differs")
				}
			}
		})
	}
}
