//go:build !windows

package sessionbroker

import (
	"context"
	"encoding/pem"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshview"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// This consumer pipeline starts at an immutable verified Node projection. Real
// signed admission and carrier ordering are covered by localmesh actor tests.
func TestOperatorRetainedRouteProjectionKeepsConsumersWithinOriginalCap(t *testing.T) {
	_, self, target := remoteTopologyFixture(t)
	now := time.Now()
	old := localmesh.RouteOrigin{Prefix: netip.PrefixFrom(target.MeshAddress(), 128), RouterID: 42, Seqno: 7}
	next := old
	next.Seqno++
	end := now.Add(5 * time.Second)
	route := babel.Route{Prefix: old.Prefix, RouterID: old.RouterID, Seqno: old.Seqno, Metric: 4096, Link: 1}
	snapshot := localmesh.NodeSnapshot{Self: self, CapturedAt: now,
		Links:   []localmesh.PeerLink{{ID: 1, Device: target, Cost: 4096, Transport: "ble"}},
		Devices: []localmesh.Manifest{{Version: localmesh.ManifestVersionRoute, Device: target.UUID(), Revision: 2, Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), AdmittedUntil: now.Add(time.Minute), Route: &next, AgentPort: 50052, AgentTransport: localmesh.AgentMTLS, Platform: localmesh.PlatformWendyOS, Services: []localmesh.ManifestService{{Mesh: "review", AppID: "com.example.review", ServiceID: "http", Type: "_http._tcp", Instance: "fixture", Port: 8080}}}},
		Routes:  []babel.Route{route}, ForwardingRoutes: []babel.Route{route},
		RouteAuthorizations: map[localmesh.RouteOrigin]bool{old: true, next: true},
		RouteProofs:         map[localmesh.RouteOrigin]localmesh.RouteProof{old: {Principal: target.Principal(), Deadline: end, Retained: true}, next: {Principal: target.Principal(), Deadline: now.Add(time.Minute)}},
	}
	if err := localmesh.CheckManifestServices(snapshot.Devices[0].Services); err != nil {
		t.Fatal(err)
	}
	certificate, _ := liteManagementCertificate(t, nil, nil, "operator", "spiffe://wendy.sh/tenant/"+self.Tenant().String()+"/operator/fixture", false)
	r := &operatorTargets{certificate: config.CertificateInfo{PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}))}, localSnapshot: func() localmesh.NodeSnapshot { return snapshot }}
	r.topology = func(context.Context) (*pb.LocalMeshTopology, error) {
		return meshview.Topology(self, snapshot, time.Now(), true), nil
	}
	r.serviceTopology = r.topology
	check := func(want bool, cap int64) {
		t.Helper()
		view := meshview.Topology(self, snapshot, now, true)
		info, err := operatorManagementTarget(view, self, target, now)
		if (err == nil) != want || want && info.Expires != cap {
			t.Fatalf("management=%+v err=%v want=%v cap=%d", info, err, want, cap)
		}
		if got := serviceOriginLive(view, self, target, view.Announcements[0], now); got != want {
			t.Fatalf("service route live=%v want=%v", got, want)
		}
		if got := len(view.Announcements[0].Services); (got == 1) != want {
			t.Fatalf("projected services=%d want=%v", got, want)
		}
		services, err := r.serviceSnapshot(context.Background(), OperatorServiceQuery{})
		if err != nil || (len(services.Services) == 1) != want || want && services.Services[0].Expires != cap {
			t.Fatalf("services=%+v err=%v want=%v cap=%d", services, err, want, cap)
		}
		directory := r.directorySnapshot(context.Background())
		if (len(directory.Devices) == 1) != want || want && (directory.Devices[0].Expires != cap || directory.Devices[0].Metric != uint32(route.Metric) || directory.Devices[0].NextHop != target.Principal()) {
			t.Fatalf("directory=%+v want=%v cap=%d", directory, want, cap)
		}
	}
	check(true, end.UnixMilli())
	proof := snapshot.RouteProofs[old]
	proof.Deadline = now
	snapshot.RouteProofs[old] = proof
	check(false, 0) // New announcement remains live but cannot renew the old route.
	proof.Deadline = end
	snapshot.RouteProofs[old] = proof
	snapshot.RouteAuthorizations[old] = false
	check(false, 0) // Withdrawal/key/scope revocation from Directory is authoritative.
	snapshot.RouteAuthorizations[old] = true
	snapshot.Routes[0].Seqno = next.Seqno
	snapshot.ForwardingRoutes[0].Seqno = next.Seqno
	delete(snapshot.RouteProofs, old)
	delete(snapshot.RouteAuthorizations, old)
	check(true, snapshot.Devices[0].AdmittedUntil.UnixMilli()) // Actual new tuple uses its own cap.
}
