package babel

import (
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel/internal/wire"
)

func TestPerLinkHelloIntervalsAndValidation(t *testing.T) {
	e := newTestEngine(t, 1)
	slow := testLink(1)
	slow.HelloInterval = DefaultHelloInterval
	fast := testLink(2)
	packets := append(step(t, e, 0, AddLink{slow}).Datagrams, step(t, e, 0, AddLink{fast}).Datagrams...)
	seenHello, seenIHU := map[LinkID]uint16{}, map[LinkID]uint16{}
	for _, p := range packets {
		messages, err := wire.Decode(p.Payload, slow.Local)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range messages {
			if m.Type == wire.Hello {
				seenHello[p.Link] = m.Interval
			}
			if m.Type == wire.IHU {
				seenIHU[p.Link] = m.Interval
			}
		}
	}
	if seenHello[1] != 400 || seenHello[2] != 100 || seenIHU[1] != 800 || seenIHU[2] != 200 {
		t.Fatalf("carrier timing leaked between links: hello=%v ihu=%v", seenHello, seenIHU)
	}
	for _, interval := range []time.Duration{-time.Second, 50 * time.Millisecond, 101 * time.Millisecond, 32768 * 10 * time.Millisecond} {
		engine := newTestEngine(t, 1)
		link := testLink(1)
		link.HelloInterval = interval
		if _, err := engine.Step(0, AddLink{link}); err != ErrConfig {
			t.Fatalf("invalid link Hello interval %s: %v", interval, err)
		}
	}
}

// AP kept authenticated CoCs and valid signed origins, but Thor had a 6.75s
// Babel transmit gap. Use actual engine packets and never remove a link: the
// former 1s interval loses its route while the existing 4s default survives.
func TestMeasuredStreamControlGapDoesNotExpireNeighbour(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval time.Duration
		want     bool
	}{{"former_one_second", 0, false}, {"stream_default", DefaultHelloInterval, true}} {
		t.Run(tc.name, func(t *testing.T) {
			engines := []*Engine{newTestEngine(t, 1), newTestEngine(t, 2)}
			prefixes := []netip.Prefix{netip.MustParsePrefix("fd00::1/128"), netip.MustParsePrefix("fd00::2/128")}
			type delivery struct {
				to     int
				packet []byte
			}
			var pending []delivery
			collect := func(from int, fx Effects) {
				for _, p := range fx.Datagrams {
					pending = append(pending, delivery{1 - from, p.Payload})
				}
			}
			for i, e := range engines {
				collect(i, step(t, e, 0, Originate{Prefix: prefixes[i]}))
				link := testLink(1)
				link.HelloInterval = tc.interval
				if i == 1 {
					link.Local, link.Peer = link.Peer, link.Local
				}
				collect(i, step(t, e, 0, AddLink{link}))
			}
			for at := time.Duration(0); at <= 8500*time.Millisecond; at += 100 * time.Millisecond {
				for i, e := range engines {
					collect(i, step(t, e, at, Tick{}))
				}
				for turns := 0; len(pending) > 0; turns++ {
					if turns > 100 {
						t.Fatal("bootstrap did not quiesce")
					}
					next := pending[0]
					pending = pending[1:]
					collect(next.to, step(t, engines[next.to], at, Receive{1, next.packet}))
				}
			}
			for i, e := range engines {
				learned := false
				for _, r := range e.Snapshot().Routes {
					if r.Prefix == prefixes[1-i] && !r.Unreachable {
						learned = true
					}
				}
				if !learned {
					t.Fatal("fixture never learned the peer route")
				}
				// Observe liveness after a gap exceeding the measured 6.75s, without
				// delivering the next batch or removing the authenticated transport.
				step(t, e, 16*time.Second, Tick{})
				reachable := false
				for _, r := range e.Snapshot().Routes {
					if r.Prefix == prefixes[1-i] && !r.Unreachable {
						reachable = true
					}
				}
				if reachable != tc.want {
					t.Fatalf("Hello=%s reachable after 7.5s=%t want=%t", tc.interval, reachable, tc.want)
				}
			}
		})
	}
}
