package backend

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"net"
	"strings"
	"testing"
	"time"
)

type settingsAgent struct {
	pb.UnimplementedWendyWiFiServiceServer
	pb.UnimplementedWendyLocalMeshAdminServiceServer
	joins   chan *pb.ConnectToWiFiRequest
	patches chan *pb.ConfigureLocalMeshRequest
}

type meshDesktopLite struct {
	*fakeDesktopLite
	status            *litepb.WendyComNetStatus
	readErr, writeErr error
	writes            int
}

func (f *meshDesktopLite) GetNetStatus(time.Duration) (*litepb.WendyComNetStatus, error) {
	return f.status, f.readErr
}
func (f *meshDesktopLite) SetNetConfig(config *litepb.WendyComNetConfig, _ time.Duration) (*litepb.WendyComNetStatus, error) {
	f.writes++
	if f.writeErr != nil {
		return nil, f.writeErr
	}
	f.status = &litepb.WendyComNetStatus{Desired: config, State: litepb.WendyComNetState_WENDY_COM_NET_RUNNING}
	return f.status, nil
}

func TestLiteMeshParticipationPreservesCarriersAndLimits(t *testing.T) {
	b := New()
	defer b.Shutdown()
	f := &meshDesktopLite{fakeDesktopLite: newFakeDesktopLite(), status: &litepb.WendyComNetStatus{
		Desired: &litepb.WendyComNetConfig{Ble: true, InfrastructureWifi: true, MaxLinks: 8, BleLinks: 3, WifiLinks: 8, NanLinks: 1,
			BleProfile: litepb.WendyComBleProfile_WENDY_COM_BLE_CODED_S2},
	}}
	// Preserve fields introduced by newer firmware, too.
	f.status.Desired.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x07})
	b.lite, b.session = f, 7
	saved := proto.Clone(f.status.Desired).(*litepb.WendyComNetConfig)
	initial, err := b.NetworkSettings(7)
	if err != nil || initial.Mesh.GetConfigured().GetParticipate() || initial.Mesh.GetState() != "off" {
		t.Fatalf("disabled settings = %v, %v", initial, err)
	}
	for _, enabled := range []uint32{1, 0} {
		if err := b.ConfigureMesh(7, 1, enabled); err != nil {
			t.Fatal(err)
		}
		want := proto.Clone(saved).(*litepb.WendyComNetConfig)
		want.Enabled = enabled != 0
		if !proto.Equal(f.status.Desired, want) {
			t.Fatalf("changed unrelated settings: %v", f.status.Desired)
		}
	}
	if saved.Enabled {
		t.Fatal("mutated the read snapshot")
	}
	value, err := b.NetworkSettings(7)
	if err != nil || value.Mesh.GetState() != "Waiting for peers" || value.Mesh.GetAuthenticatedPeers() != 0 {
		t.Fatalf("zero links reported as connected: %v, %v", value, err)
	}
	for _, session := range []uint64{0, 6} {
		if _, err := b.NetworkSettings(session); err == nil {
			t.Fatal("accepted stale read")
		}
		if err := b.ConfigureMesh(session, 1, 1); err == nil {
			t.Fatal("accepted stale write")
		}
	}
	if err := b.ConfigureMesh(7, 16, 16); err == nil {
		t.Fatal("accepted unrelated carrier change")
	}
	if f.writes != 2 {
		t.Fatal("invalid request reached device")
	}
	f.readErr = errors.New("firmware does not support mesh settings")
	if _, err := b.NetworkSettings(7); err == nil {
		t.Fatal("failed read reported disabled")
	}
	if err := b.ConfigureMesh(7, 1, 1); err == nil {
		t.Fatal("wrote without current settings")
	}
	f.readErr = nil
	f.status.Desired = nil
	if _, err := b.NetworkSettings(7); err == nil {
		t.Fatal("missing settings reported disabled")
	}
	if err := b.ConfigureMesh(7, 1, 1); err == nil {
		t.Fatal("replaced missing settings")
	}
	f.status.Desired = saved
	f.writeErr = errors.New("device rejected settings")
	if err := b.ConfigureMesh(7, 1, 1); err == nil {
		t.Fatal("write failure reported success")
	}
}

func (s *settingsAgent) GetWiFiStatus(context.Context, *pb.GetWiFiStatusRequest) (*pb.GetWiFiStatusResponse, error) {
	return &pb.GetWiFiStatusResponse{Connected: true, Ssid: proto.String("Saved")}, nil
}
func (s *settingsAgent) ListWiFiNetworks(context.Context, *pb.ListWiFiNetworksRequest) (*pb.ListWiFiNetworksResponse, error) {
	return nil, status.Error(codes.Unavailable, "scan unavailable")
}
func (s *settingsAgent) ListKnownWiFiNetworks(context.Context, *pb.ListKnownWiFiNetworksRequest) (*pb.ListKnownWiFiNetworksResponse, error) {
	return &pb.ListKnownWiFiNetworksResponse{Networks: []*pb.ListKnownWiFiNetworksResponse_KnownWiFiNetwork{{Ssid: "Saved", Security: pb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK}}}, nil
}
func (s *settingsAgent) GetLocalMeshTopology(context.Context, *pb.GetLocalMeshTopologyRequest) (*pb.LocalMeshTopology, error) {
	return &pb.LocalMeshTopology{SelfPrincipal: "self", RuntimeAvailable: true, CapturedAtUnixMs: 12345, Truncated: true,
		Peers:  []*pb.LocalMeshPeerLink{{LinkId: 1, Principal: "peer", Transport: "ble"}, {LinkId: 2, Principal: "peer", Transport: "ethernet"}},
		Routes: []*pb.LocalMeshRoute{{Prefix: "fd00::/64", NextHopPrincipal: "peer", Unreachable: true}}}, nil
}
func (s *settingsAgent) GetLocalMeshStatus(context.Context, *pb.GetLocalMeshStatusRequest) (*pb.LocalMeshStatus, error) {
	return nil, status.Error(codes.PermissionDenied, "organization access required")
}
func (s *settingsAgent) ConnectToWiFi(_ context.Context, req *pb.ConnectToWiFiRequest) (*pb.ConnectToWiFiResponse, error) {
	s.joins <- req
	if req.Ssid == "Rejected" {
		return &pb.ConnectToWiFiResponse{ErrorMessage: proto.String("authentication failed")}, nil
	}
	return &pb.ConnectToWiFiResponse{Success: true}, nil
}
func (s *settingsAgent) ConfigureLocalMesh(_ context.Context, req *pb.ConfigureLocalMeshRequest) (*pb.LocalMeshStatus, error) {
	s.patches <- req
	return &pb.LocalMeshStatus{Configured: &pb.LocalMeshConfiguration{}}, nil
}
func settingsBackend(t *testing.T) (*Backend, *settingsAgent) {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	fake := &settingsAgent{joins: make(chan *pb.ConnectToWiFiRequest, 4), patches: make(chan *pb.ConfigureLocalMeshRequest, 4)}
	pb.RegisterWendyWiFiServiceServer(server, fake)
	pb.RegisterWendyLocalMeshAdminServiceServer(server, fake)
	go server.Serve(listener)
	conn, err := grpc.NewClient("passthrough:///settings", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	b := New()
	b.conn = grpcclient.NewFromConn(conn)
	b.session = 7
	b.life, b.stop = context.WithCancel(context.Background())
	t.Cleanup(func() { b.Shutdown(); server.Stop(); listener.Close() })
	return b, fake
}
func TestSettingsPartialFailurePreservesAvailableState(t *testing.T) {
	b, _ := settingsBackend(t)
	value, err := b.NetworkSettings(7)
	if err != nil {
		t.Fatal(err)
	}
	if value.Topology.GetSelfPrincipal() != "self" || len(value.Topology.GetPeers()) != 2 || !value.Topology.GetRoutes()[0].GetUnreachable() || !value.Topology.GetTruncated() {
		t.Fatal("topology lost parallel links, route state, or truncation")
	}
	if !value.WiFi.GetConnected() || value.WiFi.GetSsid() != "Saved" || len(value.Known) != 1 {
		t.Fatal("scan or mesh failure discarded Wi-Fi state")
	}
	if !strings.Contains(value.ScanError, "scan unavailable") || !strings.Contains(value.MeshError, "organization access required") || value.Mesh != nil {
		t.Fatal("lost per-section errors")
	}
}
func TestSettingsRejectStaleSessionBeforeMutating(t *testing.T) {
	b, fake := settingsBackend(t)
	for _, session := range []uint64{0, 6} {
		if _, err := b.NetworkSettings(session); err == nil {
			t.Fatal("read accepted stale session")
		}
		if err := b.JoinWiFi(session, "Network", "test-password", 4, false); err == nil {
			t.Fatal("join accepted stale session")
		}
		if err := b.DisconnectWiFi(session); err == nil {
			t.Fatal("disconnect accepted stale session")
		}
		if err := b.ForgetWiFi(session, "Saved"); err == nil {
			t.Fatal("forget accepted stale session")
		}
		if err := b.ConfigureMesh(session, 1, 0); err == nil {
			t.Fatal("mesh accepted stale session")
		}
	}
	if len(fake.joins) != 0 || len(fake.patches) != 0 {
		t.Fatal("stale operation reached device")
	}
}
func TestWiFiJoinValidationAndSavedCredentialReuse(t *testing.T) {
	b, fake := settingsBackend(t)
	if err := b.JoinWiFi(7, "Saved", "", 4, false); err != nil {
		t.Fatal(err)
	}
	req := <-fake.joins
	if req.Security != nil || req.Hidden != nil || req.GetPassword() != "" {
		t.Fatal("saved credentials would be overwritten")
	}
	if err := b.JoinWiFi(7, "New", "", 4, false); err == nil {
		t.Fatal("new secure network accepted empty password")
	}
	if err := b.JoinWiFi(7, " ", "", 1, false); err == nil {
		t.Fatal("empty SSID accepted")
	}
	if err := b.JoinWiFi(7, strings.Repeat("x", 33), "", 1, false); err == nil {
		t.Fatal("oversized SSID accepted")
	}
	if err := b.JoinWiFi(7, "Hidden", "test-password", 5, true); err != nil {
		t.Fatal(err)
	}
	req = <-fake.joins
	if req.GetSsid() != "Hidden" || !req.GetHidden() || req.GetSecurity() != pb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA3_SAE {
		t.Fatal("join options lost")
	}
	if err := b.JoinWiFi(7, "Rejected", "test-password", 4, false); err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatal("negative agent response reported success")
	}
}
func TestMeshPatchPreservesUnchangedFields(t *testing.T) {
	b, fake := settingsBackend(t)
	if err := b.ConfigureMesh(7, 1, 0); err != nil {
		t.Fatal(err)
	}
	req := <-fake.patches
	if req.Participate == nil || *req.Participate || req.Roam != nil || req.ShareUplink != nil || req.Nan != nil || req.Ble != nil || req.Ethernet != nil || req.InfrastructureWifi != nil {
		t.Fatal("disabling mesh changed unrelated configuration")
	}
	if err := b.ConfigureMesh(7, 64, 64); err != nil {
		t.Fatal(err)
	}
	req = <-fake.patches
	if req.InfrastructureWifi == nil || !*req.InfrastructureWifi || req.Participate != nil {
		t.Fatal("carrier patch lost presence")
	}
	for _, mask := range []uint32{0, 128} {
		if err := b.ConfigureMesh(7, mask, 0); err == nil {
			t.Fatal("invalid patch accepted")
		}
	}
}
