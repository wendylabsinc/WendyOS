package services

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"google.golang.org/protobuf/proto"
)

func TestLocalMeshServicesNormalOptInLeasePortReplacementAndRouteLoss(t *testing.T) {
	self, snapshot, now := topologyFixture(t)
	service := localmesh.ManifestService{Mesh: "lab", AppID: "sh.wendy.sensor", ServiceID: "readings", Type: "_sensor._tcp", Instance: "Room sensor", Port: 8080, TXT: []string{"units=celsius"}}
	snapshot.Devices[0].Services = []localmesh.ManifestService{service}
	plain := projectLocalMeshTopology(self, snapshot, now)
	if len(plain.Announcements[0].Services) != 0 {
		t.Fatal("ordinary Nearby requested service contents")
	}
	with := projectLocalMeshTopology(self, snapshot, now, true)
	announcement := with.Announcements[0]
	if len(announcement.Services) != 1 || announcement.Services[0].Port != 8080 || announcement.Services[0].AppId != service.AppID || announcement.ExpiresAtUnixMs != snapshot.Devices[0].Expires || announcement.Revision != snapshot.Devices[0].Revision {
		t.Fatal("inherited service view", with)
	}
	snapshot.Devices[0].Services[0].Port = 8081
	snapshot.Devices[0].Revision++
	changed := projectLocalMeshTopology(self, snapshot, now, true)
	if len(changed.Announcements[0].Services) != 1 || changed.Announcements[0].Services[0].Port != 8081 || changed.Announcements[0].Revision != announcement.Revision+1 {
		t.Fatal("new manifest did not replace service endpoint")
	}
	snapshot.Routes[0].Unreachable = true
	if lost := projectLocalMeshTopology(self, snapshot, now, true); len(lost.Announcements[0].Services) != 0 {
		t.Fatal("unreachable service advertised", lost)
	}
	snapshot.Routes[0].Unreachable = false
	if expired := projectLocalMeshTopology(self, snapshot, now.Add(2*time.Minute), true); len(expired.Announcements[0].Services) != 0 {
		t.Fatal("expired inherited service lease advertised", expired)
	}
}

func TestLocalMeshServicesNormalLargeMeshResponseBound(t *testing.T) {
	self, snapshot, now := topologyFixture(t)
	base := snapshot.Devices[0]
	snapshot.Devices = nil
	snapshot.Routes = nil
	snapshot.RouteAuthorizations = make(map[localmesh.RouteOrigin]bool)
	for range 64 {
		peer := meshid.NewDevice(self.Tenant(), uuid.New())
		origin := localmesh.RouteOrigin{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128), RouterID: babel.RouterID(len(snapshot.Devices) + 7), Seqno: 9}
		manifest := base
		manifest.Device = peer.UUID()
		manifest.Route = &origin
		manifest.Services = nil
		for service := range 16 {
			manifest.Services = append(manifest.Services, localmesh.ManifestService{Mesh: "lab", AppID: "sh.wendy.sensor", ServiceID: fmt.Sprintf("sensor-%d", service), Type: "_sensor._tcp", Instance: fmt.Sprintf("Sensor %d", service), Port: uint16(8000 + service), TXT: []string{"a=" + strings.Repeat("x", 248), "b=" + strings.Repeat("y", 248), "c=" + strings.Repeat("z", 248), "d=" + strings.Repeat("w", 248)}})
		}
		if err := localmesh.CheckManifestServices(manifest.Services); err != nil {
			t.Fatal("fixture services do not fit a real manifest", err)
		}
		snapshot.Devices = append(snapshot.Devices, manifest)
		snapshot.Routes = append(snapshot.Routes, babel.Route{Prefix: origin.Prefix, RouterID: origin.RouterID, Seqno: origin.Seqno, Metric: 128, Link: 1, NextHop: netip.MustParseAddr("fe80::2")})
		snapshot.RouteAuthorizations[origin] = true
	}
	plain := projectLocalMeshTopology(self, snapshot, now)
	with := projectLocalMeshTopology(self, snapshot, now, true)
	rows := 0
	for _, announcement := range with.Announcements {
		rows += len(announcement.Services)
	}
	if !with.Truncated || rows == 0 || rows > maxTopologyEntries || proto.Size(with)-proto.Size(plain) > maxTopologyServiceBytes {
		t.Fatal("service observation exceeded response budget", rows, proto.Size(with), proto.Size(plain), with.Truncated)
	}
}
