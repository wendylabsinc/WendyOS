package main

import (
	"context"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
)

// Router-directory admission verifies manifests before this projection runs.
// Native endpoints require no separate catalogue record or inventory exchange.
func TestMeshSensorSourcesFollowRouteAndAgentPort(t *testing.T) {
	const tenant = "11111111-2222-4333-8444-555555555555"
	const device = "44444444-4444-4444-8444-444444444444"
	peer, err := meshid.DeviceFromPrincipal("spiffe://wendy.sh/tenant/" + tenant + "/device/" + device)
	if err != nil {
		t.Fatal(err)
	}
	self, err := meshid.ParseDevice(peer.Tenant(), "44444444-4444-4444-8444-444444444445")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := localmesh.NodeSnapshot{
		Self: self,
		Devices: []localmesh.Manifest{{Device: peer.UUID(), Name: "Lab C5", Expires: time.Now().Add(time.Minute).UnixMilli(), AgentPort: 5054,
			AgentTransport: localmesh.AgentMTLS, Platform: localmesh.PlatformLite, Caps: []string{localmesh.CapabilitySensors}}},
		Routes: []babel.Route{{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128)}},
	}
	check := func(port int) {
		t.Helper()
		got := meshSensorSources(snapshot)
		if len(got) != 1 || got[0].PrincipalURI != peer.Principal() || got[0].IPAddress != peer.MeshAddress().String() || got[0].Port != port || !got[0].WendyLite || !got[0].Sensorlink || !got[0].IsMTLS {
			t.Fatalf("source = %+v, want native WendyCom endpoint at port %d", got, port)
		}
	}
	check(5054)
	snapshot.Routes = nil
	if got := meshSensorSources(snapshot); len(got) != 0 {
		t.Fatalf("route absent: %+v", got)
	}
	snapshot.Routes = []babel.Route{{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128)}}
	snapshot.Devices[0].AgentPort = 5055
	check(5055)
	snapshot.Devices[0].Caps = nil // Provider stopped; native management remains.
	if got := meshSensorSources(snapshot); len(got) != 0 {
		t.Fatalf("provider absent: %+v", got)
	}
	snapshot.Devices[0].Caps = []string{localmesh.CapabilitySensors}
	snapshot.Devices[0].Expires = time.Now().Add(-time.Second).UnixMilli()
	if got := meshSensorSources(snapshot); len(got) != 0 {
		t.Fatalf("router lease expired: %+v", got)
	}
	snapshot.Devices[0].Expires = time.Now().Add(time.Minute).UnixMilli()
	snapshot.Devices[0].Withdraw = true
	if got := meshSensorSources(snapshot); len(got) != 0 {
		t.Fatalf("withdrawn provider: %+v", got)
	}
}

func TestSensorSourceDiscoveryRPCMergesSignedIdentityBeforePairing(t *testing.T) {
	const tenant = "11111111-2222-4333-8444-555555555555"
	const principal = "spiffe://wendy.sh/tenant/" + tenant + "/device/44444444-4444-4444-8444-444444444444"
	mesh := models.LANDevice{ID: "signed-id", DisplayName: "Signed camera", IPAddress: "fd45:b891:567f::5", Port: 5055,
		TenantUUID: tenant, PrincipalURI: principal, IsMTLS: true, WendyLite: true, Sensorlink: true, Caps: []string{"sensors"}}
	lan := mesh
	lan.ID, lan.DisplayName, lan.Hostname = "local-id", "LAN name", "board.local"
	lan.IPAddress, lan.Port, lan.AssetID = "192.0.2.20", 5054, 7
	var started mcusource.SensorPairing
	var address string
	store := mcusource.NewPairingStore(filepath.Join(t.TempDir(), "pairings.json"))
	service := services.NewSensorPairingService(zap.NewNop(), store, func() certs.Scope { return certs.Scope{TenantUUID: tenant} },
		func(p mcusource.SensorPairing, addr string) { started, address = p, addr }, nil, nil)
	service.SetSourceDiscovery(browseSensorSourcesWithLAN(func(context.Context, time.Duration, func(models.LANDevice) bool) ([]models.LANDevice, error) {
		return []models.LANDevice{lan}, nil
	}, func() []models.LANDevice { return []models.LANDevice{mesh} }), func() services.SensorSourceSelf { return services.SensorSourceSelf{} })
	response, err := service.DiscoverSensorSources(context.Background(), &agentpbv2.DiscoverSensorSourcesRequest{})
	if err != nil || len(response.GetSources()) != 1 {
		t.Fatalf("discovery response: %v %v", response, err)
	}
	source := response.Sources[0]
	if source.DisplayName != mesh.DisplayName || source.Hostname != "" || source.Port != 5055 || source.AssetId != 0 ||
		!source.WendyLite || !source.IsMtls || !slices.Equal(source.Caps, mesh.Caps) || !slices.Contains(source.Addresses, lan.IPAddress) {
		t.Fatalf("RPC lost signed metadata or LAN hint: %+v", source)
	}
	// The current picker sends identity and transport without an address.
	// Discovery opens no source connection; the supervisor fetches its actual
	// manifest only after this pairing has started.
	_, err = service.AddSensorPairing(context.Background(), &agentpbv2.AddSensorPairingRequest{
		SourcePrincipalUri: source.PrincipalUri, Transport: "wendycom", Name: source.DisplayName,
	})
	if err != nil || started.SourcePrincipalURI != principal || started.SourceAssetID != 0 || started.Transport != "wendycom" || address != "" {
		t.Fatalf("pairing did not preserve dynamic source identity: %+v %q %v", started, address, err)
	}
	if saved := store.List(); len(saved) != 1 || saved[0].SourcePrincipalURI != principal {
		t.Fatalf("principal pairing not persisted: %+v", saved)
	}
}
