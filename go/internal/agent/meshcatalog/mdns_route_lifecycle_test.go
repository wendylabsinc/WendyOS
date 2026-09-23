package meshcatalog

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestRouteChurnKeepsIndependentSignedServiceLeases(t *testing.T) {
	f := newFixture(t)
	viewer, cache := f.newCatalog(t, 533, "default", nil, nil)
	origin, _ := f.newCatalog(t, 535, "default", nil, nil)
	if _, err := cache.Put(f.creds[535].Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	first := testSpec()
	second := testSpec()
	second.ServiceID = "other"
	second.Instance = "Other"
	for _, spec := range []PublishSpec{first, second} {
		wire, err := origin.Publish(spec, f.now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := viewer.Accept(wire, f.now); err != nil {
			t.Fatal(err)
		}
	}
	view := bridgeRouteView(t, f.now, 535)
	bridge, err := NewMDNSBridge(AppScope{AppID: "com.wendy.browse", AppIP: net.IPv4(10, 77, 0, 2), BridgeIndex: 7},
		viewer, func(string, Record) bool { return true }, func(SignedRecord) {},
		func() localmesh.NodeSnapshot { return view })
	if err != nil {
		t.Fatal(err)
	}
	project := func(at time.Time, expected int) ([]dns.RR, []dns.RR) {
		t.Helper()
		records, err := bridge.projection(at)
		if err != nil || len(records) != expected {
			t.Fatalf("projection at %s: got %d records, error %v; want %d", at, len(records), err, expected)
		}
		return bridge.projectionDelta(records)
	}
	if added, gone := project(f.now.Add(time.Second), 7); len(added) != 7 || len(gone) != 0 {
		t.Fatalf("initial two-service projection: added %d, gone %d", len(added), len(gone))
	}
	view.Routes[0].Unreachable = true
	if added, gone := project(f.now.Add(2*time.Second), 0); len(added) != 0 || len(gone) != 7 {
		t.Fatalf("route loss did not withdraw both services and shared address: added %d, gone %d", len(added), len(gone))
	}
	// A signed withdrawal received during the outage advances only that
	// service's generation. The other lease is not republished or renewed.
	removed, err := origin.Remove(first.AppID, first.ServiceID, f.now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := viewer.Accept(removed, f.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	view.Routes[0].Unreachable = false
	added, gone := project(f.now.Add(4*time.Second), 4)
	if len(added) != 4 || len(gone) != 0 {
		t.Fatalf("remaining service did not reassert with shared address: added %d, gone %d", len(added), len(gone))
	}
	addresses := 0
	for _, rr := range added {
		if rr.Header().Ttl == 0 || rr.Header().Ttl > 86 {
			t.Fatalf("route recovery did not use remaining signed lease: %v", rr)
		}
		if _, ok := rr.(*dns.A); ok {
			addresses++
		}
	}
	if addresses != 1 {
		t.Fatalf("remaining service needs one origin address, got %d", addresses)
	}
	for _, record := range viewer.Snapshot(f.now.Add(4 * time.Second)) {
		if record.Key.ServiceID == second.ServiceID && record.Generation != 1 {
			t.Fatalf("route churn changed signed generation to %d", record.Generation)
		}
	}
	view.Routes[0].Unreachable = true
	project(f.now.Add(5*time.Second), 0)
	view.Routes[0].Unreachable = false
	if added, gone := project(f.now.Add(91*time.Second), 0); len(added) != 0 || len(gone) != 0 {
		t.Fatalf("expired service resurrected after route recovery: added %d, gone %d", len(added), len(gone))
	}
}
