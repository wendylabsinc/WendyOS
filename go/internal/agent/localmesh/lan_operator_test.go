//go:build linux || darwin

package localmesh

import (
	"context"
	"net"
	"testing"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestLANRoutedOperatorAdmission(t *testing.T) {
	a, b := testCredentials(t)
	operator, err := meshid.PeerFromPrincipal("spiffe://wendy.sh/tenant/" + a.Device.Tenant().String() + "/operator/alice")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := meshid.PeerFromPrincipal("spiffe://wendy.sh/tenant/" + testOtherTenant + "/operator/alice")
	if err != nil {
		t.Fatal(err)
	}
	_, subnet, _ := net.ParseCIDR("192.168.100.0/24")
	for _, tc := range []struct {
		name       string
		self, peer meshid.Device
		ip         string
		want       bool
	}{
		{"local devices", a.Device, b.Device, "192.168.100.110", true},
		{"routed devices", a.Device, b.Device, "192.168.0.110", false},
		{"inbound operator", a.Device, operator, "192.168.0.110", true},
		{"outbound operator", operator, a.Device, "192.168.0.110", true},
		{"foreign operator", a.Device, foreign, "192.168.0.110", false},
		{"foreign on link", a.Device, foreign, "192.168.100.110", false},
		{"self", operator, operator, "192.168.0.110", false},
		{"link local", a.Device, operator, "169.254.1.2", false},
		{"multicast", a.Device, operator, "224.0.0.251", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lanPeerAddressAllowed(tc.self, tc.peer, net.ParseIP(tc.ip), subnet); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
	// Cross-subnet TLS attempts still consume the same pre-authentication
	// connection budget. Identity-based admission follows only after TLS.
	claims := &lanClaims{policy: NewConnectionPolicy(a.Device, true, ConnectionLimits{1, 1, 1})}
	gate := lanIncomingContext(lanInterface{net: subnet}, claims)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	info := &quic.ClientInfo{RemoteAddr: &net.UDPAddr{IP: net.ParseIP("192.168.0.110"), Port: LANPort}}
	if _, err := gate(ctx, info); err != nil {
		t.Fatal(err)
	}
	if _, err := gate(ctx, info); err == nil {
		t.Fatal("routed attempts bypassed connection budget")
	}
}

func TestLANOperatorHintsRequireOperatorAndTenant(t *testing.T) {
	a, b := testCredentials(t)
	op, _ := meshid.PeerFromPrincipal("spiffe://wendy.sh/tenant/" + a.Device.Tenant().String() + "/operator/alice")
	cfg := LANConfig{Credentials: a, OperatorCandidates: func() []LANPeer {
		return []LANPeer{
			{Principal: b.Device.Principal(), IP: net.ParseIP("192.168.100.110")},
			{Principal: "spiffe://wendy.sh/tenant/" + testOtherTenant + "/device/" + b.Device.UUID().String(), IP: net.ParseIP("192.168.100.111")},
		}
	}}
	if len(operatorLANEntries(cfg)) != 0 {
		t.Fatal("device accepted operator-only hints")
	}
	cfg.Credentials = &Credentials{Device: op}
	entries := operatorLANEntries(cfg)
	if len(entries) != 1 {
		t.Fatalf("hints: %v", entries)
	}
	_, subnet, _ := net.ParseCIDR("192.168.0.0/24")
	_, keys := lanCandidateEntries(entries, op, subnet)
	if len(keys) != 1 || keys[0] != b.Device.Principal() {
		t.Fatal("routed candidate lost")
	}
}
