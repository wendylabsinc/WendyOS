package directstream

import "testing"

func TestCompleteProfileNegotiation(t *testing.T) {
	tls := CapabilityVersion | CapabilityTLS
	quic := CapabilityVersion | CapabilityQUIC
	both := tls | quic
	for _, tt := range []struct{ local, remote, want uint32 }{
		{both, both, quic}, {both, tls, tls}, {tls, quic, 0},
		{tls, 1 | CapabilityTLS, 0}, {1 | CapabilityTLS, tls, 0}, {tls, 0, 0},
	} {
		if got := NegotiatedCapability(tt.local, tt.remote); got != tt.want {
			t.Fatalf("negotiate %x/%x: got %x, want %x", tt.local, tt.remote, got, tt.want)
		}
	}
	for _, offers := range [][4]uint32{{tls, 0, tls, 0}, {0, tls, tls, 0}, {tls, 0, 0, tls}, {0, tls, 0, tls}} {
		selected, deferred := NegotiatedCarrier(offers[0], offers[1], offers[2], offers[3])
		if selected != tls || deferred != (offers[0] == 0 || offers[2] == 0) {
			t.Fatalf("carrier offers %v: selected %x, deferred %v", offers, selected, deferred)
		}
	}
}
