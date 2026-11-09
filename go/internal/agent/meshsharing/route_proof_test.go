package meshsharing

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
)

func TestGatewayRetainedRouteUsesOwnProofDeadline(t *testing.T) {
	now := time.Now()
	donor := policyDonorDevice(t)
	view := gatewayView(donor, now)
	route := &view.Routes[0]
	route.RouterID = 42
	route.Seqno = 7
	old := localmesh.RouteOrigin{Prefix: route.Prefix, RouterID: route.RouterID, Seqno: route.Seqno}
	next := old
	next.Seqno++
	view.Devices[0].Route = &next
	view.RouteAuthorizations = map[localmesh.RouteOrigin]bool{old: true, next: true}
	original := now.Add(3 * time.Second)
	view.RouteProofs = map[localmesh.RouteOrigin]localmesh.RouteProof{
		old:  {Principal: donor.Principal(), Deadline: original, Retained: true},
		next: {Principal: donor.Principal(), Deadline: now.Add(time.Minute)},
	}
	offers := []meshcatalog.Record{gatewayOffer(donor, now)}
	got := SignedGateway(policySelf(t), view, offers, now)
	if !got.Verified || !got.Expires.Equal(original) {
		t.Fatalf("replacement manifest extended old route:%+v", got)
	}
	if expired := SignedGateway(policySelf(t), view, offers, original); expired.Verified {
		t.Fatal("old proof expiry borrowed new tuple's longer lease")
	}
	view.RouteProofs[old] = localmesh.RouteProof{Principal: policySelf(t).Principal(), Deadline: original, Retained: true}
	if SignedGateway(policySelf(t), view, offers, now).Verified {
		t.Fatal("foreign proof supplied donor authority")
	}
	delete(view.RouteProofs, old)
	if SignedGateway(policySelf(t), view, offers, now).Verified {
		t.Fatal("missing exact tuple borrowed another proof")
	}
}
