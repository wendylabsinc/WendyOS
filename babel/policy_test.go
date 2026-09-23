package babel

import (
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/babel/internal/wire"
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
