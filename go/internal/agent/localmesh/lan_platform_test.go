package localmesh

import (
	"github.com/hashicorp/mdns"
	"testing"
)

func TestNormalLANPlatformTransportPlanning(t *testing.T) {
	for _, tc := range []struct {
		name, platform, role string
		tcp                  bool
	}{
		{"WendyOS router", "WendyOS", "router", false},
		{"WendyOS explicit platform", "WendyOS", "embedded-router", false},
		{"Lite router", "Lite", "embedded-router", true},
		{"Lite full router", "Lite", "router", true},
		{"legacy WendyOS", "", "router", false},
		{"legacy Lite", "", "embedded-router", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := &mdns.ServiceEntry{}
			if tc.platform != "" {
				entry.InfoFields = []string{"PlAtFoRm=" + tc.platform}
			}
			if got := shouldDialTLSLAN(entry, tc.role); got != tc.tcp {
				t.Fatalf("TCP eligible=%v, want %v", got, tc.tcp)
			}
		})
	}
}
