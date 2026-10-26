package sim

import (
	"net/netip"
	"testing"
	"time"

	babel "github.com/wendylabsinc/WendyOS/go/babel"
)

func TestRepeatedRestartDoesNotLoopThroughDefault(t *testing.T) {
	for _, recheckpoint := range []bool{false, true} {
		name := "restore"
		if recheckpoint {
			name = "restore_checkpoint_restore"
		}
		t.Run(name, func(t *testing.T) {
			s, err := New(2)
			must(t, err)
			x, y, err := s.Connect(0, 1, 96)
			must(t, err)
			must(t, s.Event(0, babel.Originate{Prefix: netip.MustParsePrefix("::/0")}))
			must(t, s.Event(1, babel.Originate{Prefix: prefix(1)}))
			must(t, s.Run(10*time.Second, nil))
			got, err := s.Walk(0, prefix(1).Addr())
			must(t, err)
			if got != 1 {
				t.Fatal("specific route did not converge", got)
			}
			cfg := babel.Config{RouterID: 2, HelloInterval: time.Second, UpdateInterval: 4 * time.Second, IPv4ViaIPv6: true}
			r, err := babel.Restore(cfg, s.Nodes[1].Checkpoint())
			must(t, err)
			if recheckpoint {
				r, err = babel.Restore(cfg, r.Checkpoint())
				must(t, err)
			}
			s.Nodes[1] = r
			s.FIB[1] = nil
			// The peer retains its specific route across the unannounced restart.
			// Reconnect with a fresh local incarnation; do not re-originate the
			// specific prefix. The restarted router will learn the peer's default.
			z := Endpoint{1, y.Link + 1}
			delete(s.Peers, y)
			s.Peers[x] = z
			s.Peers[z] = x
			must(t, s.Event(1, babel.AddLink{Link: babel.Link{ID: z.Link, Local: netip.MustParseAddr("fe80::2"), Peer: netip.MustParseAddr("fe80::1"), Cost: 96}}))
			must(t, s.Run(s.Now+3*time.Second, func(s *Network) error {
				_, err := s.Walk(0, prefix(1).Addr())
				return err
			}))
			foundDefault := false
			for _, route := range s.FIB[1] {
				if route.Prefix == netip.MustParsePrefix("::/0") && !route.Unreachable && route.Link == z.Link {
					foundDefault = true
				}
			}
			if !foundDefault {
				t.Fatal("restarted router did not learn the covering default")
			}
		})
	}
}
