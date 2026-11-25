package meshcatalog

import (
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestRetainedRouteBoundsCatalogAndDNSTTL(t *testing.T) {
	self, _ := meshid.DeviceFromPrincipal(p534)
	remote, _ := meshid.DeviceFromPrincipal(p533)
	now := time.Now().Truncate(time.Second)
	old := localmesh.RouteOrigin{Prefix: netip.PrefixFrom(remote.MeshAddress(), 128), RouterID: 42, Seqno: 7}
	next := old
	next.Seqno++
	deadline := now.Add(5 * time.Second)
	view := localmesh.NodeSnapshot{Self: self, CapturedAt: now,
		Devices:             []localmesh.Manifest{{Version: localmesh.ManifestVersionRoute, Device: remote.UUID(), Revision: 2, Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), AdmittedUntil: now.Add(time.Minute), Route: &next, Services: []localmesh.ManifestService{{Mesh: "default", AppID: "com.wendy.test", ServiceID: "http", Type: "_http._tcp", Instance: "Camera", Port: 18080}}}},
		Routes:              []babel.Route{{Prefix: old.Prefix, RouterID: old.RouterID, Seqno: old.Seqno, Metric: 4096, Link: 1}},
		RouteAuthorizations: map[localmesh.RouteOrigin]bool{old: true, next: true},
		RouteProofs:         map[localmesh.RouteOrigin]localmesh.RouteProof{old: {Principal: remote.Principal(), Deadline: deadline, Retained: true}, next: {Principal: remote.Principal(), Deadline: now.Add(time.Minute)}},
	}
	catalog, err := NewManifestCatalog("default", self, func(string, string, uint16) error { return nil }, func([]localmesh.ManifestService) error { return nil }, func() localmesh.NodeSnapshot { return view })
	if err != nil {
		t.Fatal(err)
	}
	records := catalog.Snapshot(now)
	if len(records) != 1 || !records[0].LeaseDeadline().Equal(deadline) {
		t.Fatalf("catalog borrowed current manifest deadline beyond old route: %+v", records)
	}
	if records[0].Expires != view.Devices[0].Expires {
		t.Fatal("signed metadata dates rewritten")
	}
	projected, err := ProjectDNS(records, "com.wendy.browser", func(string, Record) bool { return true }, testVIPs(t), now)
	if err != nil || len(projected) != 4 {
		t.Fatal(projected, err)
	}
	for _, rr := range projected {
		if rr.Header().Ttl != 5 {
			t.Fatalf("DNS TTL escaped old proof: %v", rr)
		}
	}
	if len(catalog.Snapshot(deadline)) != 0 || eligibleSnapshot(view, remote, deadline) {
		t.Fatal("expired old route borrowed current metadata lease")
	}
	view.Routes[0].Seqno = next.Seqno
	if got := catalog.Snapshot(now); len(got) != 1 || !got[0].LeaseDeadline().Equal(view.Devices[0].AdmittedUntil) {
		t.Fatal("actual new tuple did not switch to its own cap")
	}
	// A shorter current metadata lease remains authoritative too; the route
	// cap is an additional limit, never a new source of service lifetime.
	view.Devices[0].AdmittedUntil = now.Add(2 * time.Second)
	if got := catalog.Snapshot(now); len(got) != 1 || !got[0].LeaseDeadline().Equal(now.Add(2*time.Second)) {
		t.Fatal("route proof extended shorter current metadata")
	}
	view.Devices[0].AdmittedUntil = now.Add(time.Minute)
	for _, guard := range []string{"missing-proof", "foreign-principal", "unverified", "different-origin", "unreachable", "infinity", "withdrawn", "current-expired"} {
		t.Run(guard, func(t *testing.T) {
			originalRoute, originalManifest := view.Routes[0], view.Devices[0]
			originalProof, originalAuthorization := view.RouteProofs[next], view.RouteAuthorizations[next]
			defer func() {
				view.Routes[0], view.Devices[0] = originalRoute, originalManifest
				view.RouteProofs[next], view.RouteAuthorizations[next] = originalProof, originalAuthorization
			}()
			switch guard {
			case "missing-proof":
				delete(view.RouteProofs, next)
			case "foreign-principal":
				proof := originalProof
				proof.Principal = self.Principal()
				view.RouteProofs[next] = proof
			case "unverified":
				view.RouteAuthorizations[next] = false
			case "different-origin":
				view.Routes[0].RouterID++
			case "unreachable":
				view.Routes[0].Unreachable = true
			case "infinity":
				view.Routes[0].Metric = babel.Infinity
			case "withdrawn":
				view.Devices[0].Withdraw = true
			case "current-expired":
				view.Devices[0].AdmittedUntil = now
			}
			if len(catalog.Snapshot(now)) != 0 || eligibleSnapshot(view, remote, now) {
				t.Fatal("invalid route/current metadata retained discovery")
			}
		})
	}
}
