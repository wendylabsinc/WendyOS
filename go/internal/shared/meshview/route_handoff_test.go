package meshview

import (
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestTopologyRetainedTupleReportsItsOwnVerifiedDeadline(t *testing.T) {
	tenant, err := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	self := meshid.NewDevice(tenant, uuid.MustParse("22222222-2222-4222-8222-222222222222"))
	donor := meshid.NewDevice(tenant, uuid.MustParse("33333333-3333-4333-8333-333333333333"))
	now := time.Now()
	old := localmesh.RouteOrigin{Prefix: netip.PrefixFrom(donor.MeshAddress(), 128), RouterID: 42, Seqno: 7}
	current := old
	current.Seqno++
	end := now.Add(5 * time.Second)
	s := localmesh.NodeSnapshot{Self: self, CapturedAt: now,
		Links:               []localmesh.PeerLink{{ID: 1, Device: donor, Cost: 4096, Transport: "ble"}},
		Devices:             []localmesh.Manifest{{Version: localmesh.ManifestVersionRoute, Device: donor.UUID(), Revision: 2, Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), Route: &current}},
		Routes:              []babel.Route{{Prefix: old.Prefix, RouterID: old.RouterID, Seqno: old.Seqno, Metric: 4096, Link: 1}},
		RouteAuthorizations: map[localmesh.RouteOrigin]bool{old: true, current: true},
		RouteProofs:         map[localmesh.RouteOrigin]localmesh.RouteProof{old: {Principal: donor.Principal(), Deadline: end, Retained: true}, current: {Principal: donor.Principal(), Deadline: now.Add(time.Minute)}},
	}
	view := Topology(self, s, now)
	if len(view.Announcements) != 1 || view.Announcements[0].SequenceNumber != 8 || len(view.Routes) != 1 || view.Routes[0].SequenceNumber != 7 || !view.Routes[0].OriginAuthorized || view.Routes[0].SignedRouteExpiresAtUnixMs != end.UnixMilli() {
		t.Fatalf("retained proof projection lost its own authority:%+v", view)
	}
	if Topology(self, s, end).Routes[0].OriginAuthorized {
		t.Fatal("old proof projection inherited replacement expiry")
	}
	proof := s.RouteProofs[old]
	proof.Principal = self.Principal()
	s.RouteProofs[old] = proof
	if Topology(self, s, now).Routes[0].OriginAuthorized {
		t.Fatal("foreign compact proof authorized route")
	}
}
