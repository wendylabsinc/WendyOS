package babel

import (
	"fmt"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel/internal/wire"
)

func TestRoutePolicyAdmissionAndRevocation(t *testing.T) {
	prefix := netip.MustParsePrefix("fd00::2/128")
	allowed := false
	e, err := New(Config{RouterID: 1, AcceptRoute: func(id RouterID, p netip.Prefix) bool { return allowed && id == 2 && p == prefix }})
	if err != nil {
		t.Fatal(err)
	}
	step(t, e, 0, AddLink{testLink(1)})
	reachable(t, e, 1)
	update := wire.Message{Type: wire.Update, Prefix: prefix, RouterID: 2, Seqno: 1, Metric: 0, Interval: 100, Address: testLink(1).Peer}
	receiveMessages(t, e, 0, 1, update)
	if len(e.Snapshot().Routes) != 0 || e.Snapshot().Counters.Rejected == 0 {
		t.Fatal("denied route admitted")
	}
	allowed = true
	receiveMessages(t, e, 0, 1, update)
	routes := e.Snapshot().Routes
	if len(routes) != 1 || routes[0].Unreachable {
		t.Fatal("authorized route missing", routes)
	}
	allowed = false
	step(t, e, time.Millisecond, Tick{})
	routes = e.Snapshot().Routes
	if len(routes) != 1 || !routes[0].Unreachable {
		t.Fatal("revoked route not withdrawn safely", routes)
	}
}

func TestRouteUpdatePolicyRejectsBeforeChangingState(t *testing.T) {
	prefix := netip.MustParsePrefix("fd00::2/128")
	allowed := false
	e, err := New(Config{
		RouterID: 1,
		AcceptRouteUpdate: func(id RouterID, p netip.Prefix, seq uint16) bool {
			return allowed && id == 2 && p == prefix && seq == 42
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	step(t, e, 0, AddLink{testLink(1)})
	step(t, e, 0, AddLink{testLink(2)})
	reachable(t, e, 1)
	update := wire.Message{Type: wire.Update, Prefix: prefix, RouterID: 2, Seqno: 42, Metric: 0, Interval: 400}
	receiveMessages(t, e, 0, 1, update)
	if len(e.routes) != 0 || len(e.sources) != 0 || len(e.selected) != 0 {
		t.Fatal("unsigned update changed routing state", e.Snapshot())
	}

	allowed = true
	receiveMessages(t, e, 0, 1, update)
	step(t, e, 100*time.Millisecond, Tick{}) // Advertise and establish feasibility history.
	key := routeKey{prefix, 1}
	wantCandidate := *e.routes[key]
	wantSource := *e.sources[sourceKey{prefix, 2}]
	wantSelected := e.selected[prefix]
	for _, tc := range []struct {
		name   string
		mutate func(*wire.Message)
	}{
		{"changed sequence", func(m *wire.Message) { m.Seqno++ }},
		{"changed origin", func(m *wire.Message) { m.RouterID++ }},
		{"changed prefix", func(m *wire.Message) { m.Prefix = netip.MustParsePrefix("fd00::/64") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forged := update
			tc.mutate(&forged)
			rejected := e.counters.Rejected
			receiveMessages(t, e, 100*time.Millisecond, 1, forged)
			if e.counters.Rejected != rejected+1 || len(e.routes) != 1 || len(e.sources) != 1 || len(e.requests) != 0 {
				t.Fatal("unauthorized update changed state", e.Snapshot())
			}
			if !reflect.DeepEqual(*e.routes[key], wantCandidate) || !reflect.DeepEqual(*e.sources[sourceKey{prefix, 2}], wantSource) || e.selected[prefix] != wantSelected {
				t.Fatal("unauthorized update poisoned an existing route", e.Snapshot())
			}
		})
	}
}

func TestRouteUpdatePolicyExpiresAndRenews(t *testing.T) {
	prefix := netip.MustParsePrefix("fd00::2/128")
	policyTime := time.Duration(0)
	expires := time.Second
	seq := uint16(42)
	e, err := New(Config{
		RouterID: 1,
		AcceptRouteUpdate: func(id RouterID, p netip.Prefix, s uint16) bool {
			return id == 2 && p == prefix && s == seq && policyTime < expires
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	step(t, e, 0, AddLink{testLink(1)})
	step(t, e, 0, AddLink{testLink(2)})
	reachable(t, e, 1)
	update := wire.Message{Type: wire.Update, Prefix: prefix, RouterID: 2, Seqno: seq, Metric: 0, Interval: 400}
	receiveMessages(t, e, 0, 1, update)
	step(t, e, 100*time.Millisecond, Tick{})
	if e.selected[prefix].Unreachable {
		t.Fatal("authorized route not installed")
	}

	policyTime = expires
	fx := step(t, e, policyTime, Tick{})
	if fx.Revision == 0 || !e.selected[prefix].Unreachable {
		t.Fatal("policy expiry did not remove forwarding on Tick", fx, e.Snapshot())
	}
	datagrams := fx.Datagrams
	fx = step(t, e, policyTime+100*time.Millisecond, Tick{})
	datagrams = append(datagrams, fx.Datagrams...)
	var retracted bool
	for _, d := range datagrams {
		messages, err := wire.Decode(d.Payload, e.links[d.Link].Local)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range messages {
			retracted = retracted || m.Type == wire.Update && m.Prefix == prefix && m.Metric == Infinity
		}
	}
	if !retracted {
		t.Fatal("expiry suppressed local withdrawal propagation", fx)
	}

	// Renewal for the retained candidate can restore it without another packet.
	expires = 2 * time.Second
	fx = step(t, e, policyTime+100*time.Millisecond, Tick{})
	if fx.Revision == 0 || e.selected[prefix].Unreachable || e.selected[prefix].Seqno != seq {
		t.Fatal("renewed authorization did not restore route", fx, e.Snapshot())
	}

	// A new signed sequence invalidates the old candidate until its update arrives.
	seq++
	step(t, e, policyTime+100*time.Millisecond, Tick{})
	if !e.selected[prefix].Unreachable {
		t.Fatal("old sequence survived authorization replacement")
	}
	update.Seqno = seq
	receiveMessages(t, e, policyTime+100*time.Millisecond, 1, update)
	if e.selected[prefix].Unreachable || e.selected[prefix].Seqno != seq {
		t.Fatal("new signed sequence did not restore route", e.Snapshot())
	}
}

func TestRouteUpdatePolicyRetractionOnlyWithdrawsSender(t *testing.T) {
	for _, wildcard := range []bool{false, true} {
		t.Run(fmt.Sprintf("wildcard=%t", wildcard), func(t *testing.T) {
			prefix := netip.MustParsePrefix("fd00::2/128")
			e, err := New(Config{
				RouterID: 1,
				AcceptRouteUpdate: func(id RouterID, p netip.Prefix, seq uint16) bool {
					return id == 2 && p == prefix && seq == 42
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []LinkID{1, 2, 3} {
				step(t, e, 0, AddLink{testLink(id)})
				reachable(t, e, id)
			}
			update := wire.Message{Type: wire.Update, Prefix: prefix, RouterID: 2, Seqno: 42, Metric: 0, Interval: 400}
			receiveMessages(t, e, 0, 1, update)
			receiveMessages(t, e, 0, 2, update)
			step(t, e, 100*time.Millisecond, Tick{})
			wantSource := *e.sources[sourceKey{prefix, 2}]
			wantHonest := *e.routes[routeKey{prefix, 2}]

			// A retraction does not prove an origin. It may claim any sequence,
			// but only withdraws the sending neighbour's candidate.
			receiveMessages(t, e, 100*time.Millisecond, 1, wire.Message{
				Type: wire.Update, Prefix: prefix, Wildcard: wildcard,
				RouterID: 99, Seqno: 43, Metric: Infinity, Interval: 400,
			})
			if got := e.selected[prefix]; got.Unreachable || got.Link != 2 || got.Seqno != 42 {
				t.Fatal("retraction removed an independent honest route", got)
			}
			if *e.routes[routeKey{prefix, 2}] != wantHonest || *e.sources[sourceKey{prefix, 2}] != wantSource {
				t.Fatal("retraction changed other candidate or feasibility history", e.Snapshot())
			}
		})
	}
}

func TestRoutePoliciesBothApply(t *testing.T) {
	prefix := netip.MustParsePrefix("fd00::2/128")
	e, err := New(Config{
		RouterID:          1,
		AcceptRoute:       func(RouterID, netip.Prefix) bool { return false },
		AcceptRouteUpdate: func(RouterID, netip.Prefix, uint16) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	step(t, e, 0, AddLink{testLink(1)})
	reachable(t, e, 1)
	receiveMessages(t, e, 0, 1, wire.Message{Type: wire.Update, Prefix: prefix, RouterID: 2, Seqno: 42, Metric: 0, Interval: 400})
	if len(e.routes) != 0 || len(e.selected) != 0 {
		t.Fatal("sequence policy bypassed prefix policy", e.Snapshot())
	}
}
