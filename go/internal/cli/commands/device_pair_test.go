package commands

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// legacyPairingServer is an agent that predates DiscoverSensorSources: it
// answers the other pairing RPCs, and gRPC reports the new one unimplemented.
type legacyPairingServer struct {
	agentpbv2.UnimplementedWendySensorPairingServiceServer
}

// discoveringPairingServer is a current agent reporting one source it sees.
type discoveringPairingServer struct {
	agentpbv2.UnimplementedWendySensorPairingServiceServer
}

func (discoveringPairingServer) DiscoverSensorSources(context.Context, *agentpbv2.DiscoverSensorSourcesRequest) (*agentpbv2.DiscoverSensorSourcesResponse, error) {
	return &agentpbv2.DiscoverSensorSourcesResponse{Sources: []*agentpbv2.DiscoveredSensorSource{
		{Id: "hub", DisplayName: "hub", IpAddress: "10.0.0.7", Port: 50052, IsMtls: true, AssetId: 7, OrgId: 2, Caps: []string{"sensors"}},
	}}, nil
}

func pairingClientOver(t *testing.T, srv agentpbv2.WendySensorPairingServiceServer) agentpbv2.WendySensorPairingServiceClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	agentpbv2.RegisterWendySensorPairingServiceServer(server, srv)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///sensor-pairing", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return agentpbv2.NewWendySensorPairingServiceClient(conn)
}

// Over real gRPC, an old agent's answer is exactly what triggers the local
// fallback, and a current agent's sources survive the wire.
func TestSensorPairDiscoveryOverGRPC(t *testing.T) {
	local := []models.DiscoveredDevice{{DisplayName: "laptop-side", Sensorlink: true}}
	h := &sensorPairHandler{discoverLocal: func(context.Context) ([]models.DiscoveredDevice, error) { return local, nil }}

	h.client = pairingClientOver(t, legacyPairingServer{})
	devices, localNetwork, err := h.discoverSources(context.Background())
	if err != nil || !localNetwork || len(devices) != 1 || devices[0].DisplayName != "laptop-side" {
		t.Fatalf("old agent: got %+v local=%v err=%v; want the labeled local scan", devices, localNetwork, err)
	}

	h.client = pairingClientOver(t, discoveringPairingServer{})
	devices, localNetwork, err = h.discoverSources(context.Background())
	if err != nil || localNetwork || len(devices) != 1 {
		t.Fatalf("current agent: got %+v local=%v err=%v", devices, localNetwork, err)
	}
	if d := devices[0]; d.AssetID != 7 || d.OrgID != 2 || d.IPAddress != "10.0.0.7" || d.Port() != 50052 || transportForDevice(d) != "grpc" {
		t.Fatalf("device source lost fields over the wire: %+v", d)
	}
}

func TestTransportForDevice(t *testing.T) {
	agentDev := models.DiscoveredDevice{Sensorlink: true, IsMTLS: true, Caps: []string{"sensors"}, AssetID: 5}
	if transportForDevice(agentDev) != "grpc" {
		t.Fatal("agent source should be grpc")
	}
	mcuDev := models.DiscoveredDevice{Sensorlink: true, IsMTLS: false, AssetID: 6} // legacy tcp sensorlink
	if transportForDevice(mcuDev) != "tcp" {
		t.Fatal("legacy/MCU source should be tcp")
	}
	// A Wendy Lite board is always wendycom, regardless of IsMTLS/Caps — it
	// never speaks gRPC or raw sensorlink TCP.
	liteDev := models.DiscoveredDevice{WendyLite: true, Sensorlink: true, IsMTLS: true, Caps: []string{"sensors"}, AssetID: 7}
	if transportForDevice(liteDev) != "wendycom" {
		t.Fatal("Wendy Lite source should be wendycom")
	}
}

func TestOrgAllowedRejectsMismatch(t *testing.T) {
	cli := map[int32]bool{3: true}
	if err := orgAllowed(cli, 3); err != nil {
		t.Fatalf("same org should pass: %v", err)
	}
	if err := orgAllowed(cli, 9); err == nil {
		t.Fatal("cross-org should fail")
	}
}

// With several sessions the user should be able to pair a device in ANY of
// their orgs, not just the default one — the multi-session bug this fixes.
func TestOrgAllowedAcceptsAnyLoggedInOrg(t *testing.T) {
	cli := map[int32]bool{2: true, 77: true}
	if err := orgAllowed(cli, 2); err != nil {
		t.Fatalf("device in org 2 must be allowed when logged in to {2,77}: %v", err)
	}
	if err := orgAllowed(cli, 77); err != nil {
		t.Fatalf("device in org 77 must be allowed when logged in to {2,77}: %v", err)
	}
	err := orgAllowed(cli, 9)
	if err == nil {
		t.Fatal("device in org 9 must be rejected when logged in to {2,77}")
	}
	// The error must name both the device's org and the orgs the CLI holds.
	for _, want := range []string{"9", "2", "77"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %s", want, err.Error())
		}
	}
}

// Errors name organizations the way people know them, ID secondary, when this
// machine has seen the name (WDY-3797); unknown ones keep the bare ID.
func TestOrgAllowedNamesKnownOrganizations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cacheCloudOrganizationName("prod:443", "3", "Robotics")
	cacheCloudOrganizationName("prod:443", "9", "Wendy Labs")
	err := orgAllowed(map[int32]bool{3: true, 4: true}, 9)
	if err == nil {
		t.Fatal("cross-org should fail")
	}
	want := `device is in organization "Wendy Labs" (9), but you are logged in to organizations "Robotics" (3), 4`
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
}
