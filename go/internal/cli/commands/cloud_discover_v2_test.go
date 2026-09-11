package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type discoveryV2Server struct {
	cloudpbv2.UnimplementedAssetServiceServer
	t     *testing.T
	count int32
	fail  bool
}

func discoveryV2Auth(t *testing.T, count int32, fail bool) *config.AuthConfig {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	cloudpbv2.RegisterAssetServiceServer(srv, &discoveryV2Server{t: t, count: count, fail: fail})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = lis.Addr().String()
	// Direct PKI login carries a tenant UUID, with no legacy numeric org ID.
	auth.Certificates[0].OrganizationID = 0
	return auth
}

// Drive the actual command used by Bubble Tea against a server that registers
// only v2. JSON coverage alone missed the interactive scan's v1 RPC.
func TestCloudDiscoverInteractiveV2(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprint("all=", all), func(t *testing.T) {
			auth := discoveryV2Auth(t, 205, false)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			m := newCloudDiscoverModel(ctx, auth, "", all, false, nil)
			updated, _ := m.Update(m.Init()())
			m = updated.(cloudDiscoverModel)
			if m.err != nil || len(m.devices) != 205 {
				t.Fatalf("interactive scan: %v, %d devices", m.err, len(m.devices))
			}
			wantName := "online-device"
			if all {
				wantName = "offline-device"
			}
			if !strings.Contains(m.View(), wantName) || strings.Contains(m.View(), "Error:") {
				t.Fatalf("unexpected view: %s", m.View())
			}
			id := m.devices[1].key
			updated, _ = m.Update(cloudAssetVersionMsg{assetID: id, resp: &agentpb.GetAgentVersionResponse{Version: "9.9.9"}})
			m = updated.(cloudDiscoverModel)
			if m.versions[m.devices[0].key] != nil || m.versions[id].GetVersion() != "9.9.9" {
				t.Fatal("version results mixed device UUIDs")
			}

			var copied string
			oldClipboard := clipboardWriter
			clipboardWriter = func(s string) error { copied = s; return nil }
			t.Cleanup(func() { clipboardWriter = oldClipboard })
			m.table.SetCursor(1)
			updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			m = updated.(cloudDiscoverModel)
			var info cloudDiscoverV2Info
			if err := json.Unmarshal([]byte(copied), &info); err != nil {
				t.Fatal(err)
			}
			if info.ID != id || info.Version != "9.9.9" {
				t.Fatalf("clipboard lost UUID/version: %s", copied)
			}
			updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
			m = updated.(cloudDiscoverModel)
			var infos []cloudDiscoverV2Info
			if err := json.Unmarshal([]byte(copied), &infos); err != nil {
				t.Fatal(err)
			}
			if len(infos) != 205 || infos[1].ID != id {
				t.Fatal("copy all lost devices or UUIDs")
			}

			// Subsequent refreshes must use the same version-aware fetcher.
			updated, _ = m.Update(m.scanCmd()())
			m = updated.(cloudDiscoverModel)
			if m.err != nil || len(m.devices) != 205 {
				t.Fatalf("refresh failed: %v", m.err)
			}
			updated, _ = m.Update(discoverUpdateDoneMsg{cloudAssetKey: id, deviceName: wantName})
			m = updated.(cloudDiscoverModel)
			if _, exists := m.versions[id]; exists {
				t.Fatal("updated device retained stale version")
			}
		})
	}
}

func TestCloudDiscoverInteractiveV2EmptyAndError(t *testing.T) {
	for _, fail := range []bool{false, true} {
		auth := discoveryV2Auth(t, int32(0), false)
		if fail {
			auth = discoveryV2Auth(t, 2, true)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		m := newCloudDiscoverModel(ctx, auth, "", false, false, nil)
		updated, _ := m.Update(m.Init()())
		cancel()
		m = updated.(cloudDiscoverModel)
		want := "No online devices found"
		if fail {
			want = "membership required"
		}
		if !m.hasResults || !strings.Contains(m.View(), want) || len(m.devices) != 0 {
			t.Fatalf("unexpected scan state: %s", m.View())
		}
	}
}

func TestDevicePickerSelectsV2UUID(t *testing.T) {
	auth := discoveryV2Auth(t, 2, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m := newDevicePickerModel(ctx, tui.NewPicker(), auth, 0)
	m.active = devicePickerCloudTab
	updated, _ := m.Update(devicePickerCloudMsg{msg: m.cloud.scanCmd()()})
	m = updated.(devicePickerModel)
	m.cloud.table.SetCursor(1)
	want := m.cloud.devices[1].key
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(devicePickerModel)
	choice, ok := m.choice()
	if !ok || choice.Cloud != nil || choice.CloudV2.GetId() != want {
		t.Fatalf("picker lost v2 identity: %+v", choice)
	}
}

type discoveryV2Broker struct {
	cloudpbv2.UnimplementedTunnelBrokerServiceServer
	opened chan *cloudpbv2.ClientTunnelOpen
}

func (s *discoveryV2Broker) ClientTunnel(stream grpc.BidiStreamingServer[cloudpbv2.ClientTunnelMessage, cloudpbv2.TunnelData]) error {
	msg, err := stream.Recv()
	if err != nil {
		return err
	}
	s.opened <- msg.GetOpen()
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		data := msg.GetData()
		if data == nil {
			return status.Error(codes.InvalidArgument, "expected tunnel data")
		}
		if err := stream.Send(data); err != nil {
			return err
		}
		if data.GetHalfClose() {
			return nil
		}
	}
}

func TestCloudDiscoveryV2DoesNotUseRetiredRelay(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	broker := &discoveryV2Broker{opened: make(chan *cloudpbv2.ClientTunnelOpen, 1)}
	cloudpbv2.RegisterTunnelBrokerServiceServer(srv, broker)
	go srv.Serve(lis)
	defer srv.Stop()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	auth := oidcEnrollmentAuth(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := "00000000-0000-4000-8000-000000000042"
	d := cloudDiscoveryDevice{v2: &cloudpbv2.Asset{Id: id}, key: id}
	_, err = d.openTunnel(ctx, conn, auth, 50052)
	if err == nil || !strings.Contains(err.Error(), "ML-DSA operator request-signing certificate") {
		t.Fatalf("unexpected signing capability result: %v", err)
	}
	select {
	case <-broker.opened:
		t.Fatal("PKI session contacted the retired UUID relay RPC")
	default:
	}

}

func (s *discoveryV2Server) ListAssets(req *cloudpbv2.ListAssetsRequest, stream grpc.ServerStreamingServer[cloudpbv2.ListAssetsResponse]) error {
	if req.GetOrganizationId() != testOperatorTenant || !req.GetIsComputeDevice() || req.GetLimit() != 200 {
		s.t.Errorf("unexpected request: %v", req)
	}
	md, _ := metadata.FromIncomingContext(stream.Context())
	if got := md.Get("authorization"); len(got) != 1 || got[0] != "Bearer test-access-token" {
		s.t.Error("missing bearer authentication")
	}
	if req.OnlineOnly != nil && !req.GetOnlineOnly() {
		s.t.Error("all-device queries should omit online_only")
	}
	// Cap below the requested limit to exercise server-side pagination.
	for i := req.GetOffset(); i < s.count && i < req.GetOffset()+73; i++ {
		osType, arch := "darwin", "arm64"
		name := "offline-device"
		if req.GetOnlineOnly() {
			name = "online-device"
		}
		if err := stream.Send(&cloudpbv2.ListAssetsResponse{
			Asset: &cloudpbv2.Asset{
				Id:   fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1),
				Name: name, OsType: &osType, Architecture: &arch,
			}, Total: s.count,
		}); err != nil {
			return err
		}
		if s.fail {
			return status.Error(codes.PermissionDenied, "membership required")
		}
	}
	return nil
}

func TestCloudDiscoverJSONV2(t *testing.T) {
	for _, tc := range []struct {
		name      string
		count     int32
		all, fail bool
		wantErr   string
	}{
		{name: "online", count: 205},
		{name: "all", count: 1, all: true},
		{name: "empty"},
		{name: "stream failure", count: 2, fail: true, wantErr: "membership required"},
		{name: "fleet limit", count: maxCloudAssets + 1, wantErr: "more than 10000 devices"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := grpc.NewServer()
			cloudpbv2.RegisterAssetServiceServer(srv, &discoveryV2Server{t: t, count: tc.count, fail: tc.fail})
			go srv.Serve(lis)
			t.Cleanup(srv.Stop)
			auth := oidcEnrollmentAuth(t)
			auth.CloudGRPC = lis.Addr().String()
			auth.Certificates[0].OrganizationID = 0
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var callErr error
			out := captureStdout(t, func() { callErr = cloudDiscoverJSON(ctx, auth, tc.all) })
			if tc.wantErr != "" {
				if callErr == nil || !strings.Contains(callErr.Error(), tc.wantErr) {
					t.Fatalf("error = %v", callErr)
				}
				if out != "" {
					t.Fatalf("printed partial results: %s", out)
				}
				return
			}
			if callErr != nil {
				t.Fatal(callErr)
			}
			var infos []cloudDiscoverV2Info
			if err := json.Unmarshal([]byte(out), &infos); err != nil {
				t.Fatal(err)
			}
			if infos == nil || len(infos) != int(tc.count) {
				t.Fatalf("got %d results (nil=%v), want %d", len(infos), infos == nil, tc.count)
			}
			for i, info := range infos {
				wantID := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
				wantName := "online-device"
				if tc.all {
					wantName = "offline-device"
				}
				if info.ID != wantID || info.Type != "macOS (arm64)" || info.Name != wantName {
					t.Fatalf("unexpected info: %+v", info)
				}
			}
		})
	}
}

type discoveryV1Server struct {
	cloudpb.UnimplementedAssetServiceServer
}

func (s *discoveryV1Server) ListAssets(req *cloudpb.ListAssetsRequest, stream grpc.ServerStreamingServer[cloudpb.ListAssetsResponse]) error {
	return stream.Send(&cloudpb.ListAssetsResponse{Asset: &cloudpb.Asset{Id: 77}, Total: 1})
}

func TestCloudDiscoverJSONLegacy(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	cloudpb.RegisterAssetServiceServer(srv, &discoveryV1Server{})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	auth := fakeAuth(t)
	auth.CloudGRPC = lis.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var callErr error
	out := captureStdout(t, func() { callErr = cloudDiscoverJSON(ctx, auth, false) })
	if callErr != nil {
		t.Fatal(callErr)
	}
	var infos []discoverDeviceInfo
	if err := json.Unmarshal([]byte(out), &infos); err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != 77 {
		t.Fatalf("unexpected legacy results: %s", out)
	}
}

func TestCloudPickerUsesV2AndPreservesUUID(t *testing.T) {
	auth := discoveryV2Auth(t, 1, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	devices, err := fetchCloudDiscoveryDevices(ctx, auth, true)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := pickCloudDiscoveryDevice(ctx, auth, devices[0].key, "")
	if err != nil {
		t.Fatal(err)
	}
	if selected.v2 == nil || selected.key != devices[0].key || selected.legacy != nil {
		t.Fatal("lost UUID or selected legacy API")
	}
	_, err = pickCloudDiscoveryDevice(ctx, auth, "unknown", "")
	if err == nil {
		t.Fatal("selected unrelated asset")
	}
	duplicates := []cloudDiscoveryDevice{selected, selected}
	duplicates[1].key = "other"
	if _, err = resolveCloudDiscoveryDevice(duplicates, selected.GetName()); err == nil {
		t.Fatal("accepted ambiguous name")
	}
	if got, err := resolveCloudDiscoveryDevice(duplicates, selected.key); err != nil || got.key != selected.key {
		t.Fatal("exact UUID did not resolve")
	}
}
