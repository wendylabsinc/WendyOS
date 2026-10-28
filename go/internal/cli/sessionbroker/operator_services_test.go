//go:build !windows

package sessionbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func TestOperatorServicesNormalInheritedLeasePortRefreshAndRouteLoss(t *testing.T) {
	r, cc, activity, mutate := liteManagementFixture(t)
	r.serviceTopology = r.topology
	mutate(func(view *pb.LocalMeshTopology) {
		view.ServicesIncluded = true
		view.Announcements[0].Services = []*pb.LocalMeshService{{Mesh: "lab", AppId: "sh.wendy.sensor", ServiceId: "temperature", Type: "_sensor._tcp", Instance: "Room sensor", Port: 8080, Txt: []string{"units=celsius"}}}
	})
	read := func(query OperatorServiceQuery) OperatorServices {
		t.Helper()
		encoded, _ := json.Marshal(query)
		request, reply := rawMessage(encoded), rawMessage{}
		if err := cc.Invoke(context.Background(), operatorServicesMethod, &request, &reply, grpc.ForceCodec(rawCodec{})); err != nil {
			t.Fatal(err)
		}
		var out OperatorServices
		if err := json.Unmarshal(reply, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	initial := read(OperatorServiceQuery{Mesh: "lab", Type: "_sensor._tcp"})
	if initial.State != "ready" || len(initial.Services) != 1 || initial.Services[0].Platform != "Lite" || initial.Services[0].Port != 8080 || initial.Services[0].Expires <= 0 {
		t.Fatal(initial)
	}
	mutate(func(view *pb.LocalMeshTopology) {
		view.Announcements[0].Services[0].Port = 8081
		view.Announcements[0].Revision++
	})
	changed := read(OperatorServiceQuery{})
	if len(changed.Services) != 1 || changed.Services[0].Port != 8081 || changed.Services[0].Revision != initial.Services[0].Revision+1 || changed.Services[0].Expires != initial.Services[0].Expires {
		t.Fatal("manifest did not replace inherited service", changed)
	}
	if other := read(OperatorServiceQuery{Type: "_http._tcp"}); len(other.Services) != 0 {
		t.Fatal("service type filter", other)
	}
	mutate(func(view *pb.LocalMeshTopology) { view.Routes[0].Unreachable = true })
	if lost := read(OperatorServiceQuery{}); lost.State != "ready" || len(lost.Services) != 0 {
		t.Fatal("unreachable service stayed visible", lost)
	}
	mutate(func(view *pb.LocalMeshTopology) { view.Routes[0].Unreachable = false; view.ServicesIncluded = false })
	if unsupported := read(OperatorServiceQuery{}); unsupported.State != "unsupported" {
		t.Fatal("missing optional API looked like healthy empty inventory", unsupported)
	}
	if activity.activeRPCs.Load() != 0 {
		t.Fatal("service read held user lease")
	}
}

func TestOperatorServicesNormal64DeviceObservationRemainsBounded(t *testing.T) {
	r, _, _, mutate := liteManagementFixture(t)
	fixtureLocalOperatorReporter(t, r, mutate, meshoperator.MaxPrincipalBytes)
	r.serviceTopology = r.topology
	mutate(func(view *pb.LocalMeshTopology) {
		originalHint, originalRoute := view.Announcements[0], view.Routes[0]
		view.Announcements = nil
		view.Routes = nil
		view.ServicesIncluded = true
		tenant, _ := meshid.ParseTenant(view.SelfTenantId)
		for range 64 {
			device := meshid.NewDevice(tenant, uuid.New())
			hint := proto.Clone(originalHint).(*pb.LocalMeshAnnouncement)
			hint.Principal = device.Principal()
			hint.DeviceId = device.UUID().String()
			hint.Prefix = device.MeshAddress().String() + "/128"
			for service := range 16 {
				hint.Services = append(hint.Services, &pb.LocalMeshService{Mesh: "lab", AppId: "sh.wendy.sensor", ServiceId: fmt.Sprintf("sensor-%d", service), Type: "_sensor._tcp", Instance: fmt.Sprintf("Sensor %d", service), Port: uint32(8080 + service), Txt: []string{"units=" + strings.Repeat("c", 220)}})
			}
			route := proto.Clone(originalRoute).(*pb.LocalMeshRoute)
			route.Prefix = hint.Prefix
			route.OriginPrincipal = hint.Principal
			view.Announcements = append(view.Announcements, hint)
			view.Routes = append(view.Routes, route)
		}
	})
	out, err := r.serviceSnapshot(context.Background(), OperatorServiceQuery{})
	encoded, marshalErr := json.Marshal(out)
	if err != nil || marshalErr != nil || out.State != "ready" || len(out.Services) == 0 || len(out.Services) > operatorServicesRows || !out.Truncated || len(encoded) > operatorServicesBytes {
		t.Fatal("bounded service result", len(out.Services), len(encoded), out.Truncated, err, marshalErr)
	}
}
