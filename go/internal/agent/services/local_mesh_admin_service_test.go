package services

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestLocalMeshAdminAuthorizationAndPartialUpdates(t *testing.T) {
	dir := t.TempDir()
	org, asset := int32(64), int32(445)
	svc := NewLocalMeshAdminService(nil, dir, func() (int32, int32) { return org, asset })
	request := &pb.ConfigureLocalMeshRequest{Participate: proto.Bool(true), Nan: proto.Bool(true)}
	for _, ctx := range []context.Context{context.Background(), ctxWithIdentity(t, "urn:wendy:org:64:asset:445"), ctxWithIdentity(t, "urn:wendy:org:65:user:bob")} {
		if _, err := svc.ConfigureLocalMesh(ctx, request); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("configure authorization: %v", err)
		}
		if _, err := svc.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("status authorization: %v", err)
		}
	}
	ctx := ctxWithIdentity(t, "urn:wendy:org:64:user:alice")
	if _, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty update accepted: %v", err)
	}
	if _, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Roam: proto.Bool(true)}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("roam without participate accepted: %v", err)
	}
	statusBefore, err := svc.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{})
	if err != nil || statusBefore.GetRuntimeAvailable() || statusBefore.GetConfigured().GetNan() {
		t.Fatalf("incorrect disabled default: %+v %v", statusBefore, err)
	}
	if _, err := svc.ConfigureLocalMesh(ctx, request); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, update := range []*pb.ConfigureLocalMeshRequest{{Roam: proto.Bool(true)}, {ShareUplink: proto.Bool(true)}, {Ble: proto.Bool(true)}, {Ethernet: proto.Bool(true)}, {InfrastructureWifi: proto.Bool(true)}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.ConfigureLocalMesh(ctx, update); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := svc.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{})
	if err != nil || !got.GetConfigured().GetParticipate() || !got.GetConfigured().GetRoam() ||
		!got.GetConfigured().GetShareUplink() || !got.GetConfigured().GetNan() || !got.GetConfigured().GetBle() ||
		!got.GetConfigured().GetEthernet() || !got.GetConfigured().GetInfrastructureWifi() {
		t.Fatalf("partial settings lost: %+v %v", got, err)
	}
	if _, err = svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Participate: proto.Bool(false)}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("non-atomic disable accepted: %v", err)
	}
	if _, err = svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Nan: proto.Bool(false)}); err != nil {
		t.Fatal(err)
	}
	got, err = svc.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{})
	if err != nil || got.GetConfigured().GetNan() || !got.GetConfigured().GetBle() ||
		!got.GetConfigured().GetEthernet() || !got.GetConfigured().GetInfrastructureWifi() {
		t.Fatalf("BLE inferred from NAN toggle: %+v %v", got, err)
	}
	svc.SetRuntimeStatusSource(func() LocalMeshRuntimeStatus {
		return LocalMeshRuntimeStatus{Available: true, State: "roaming", Detail: "using verified donor", AuthenticatedPeers: 3, GatewayAsset: 460}
	})
	got, err = svc.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{})
	if err != nil || !got.GetRuntimeAvailable() || got.GetGatewayAssetId() != 460 || got.GetAuthenticatedPeers() != 3 {
		t.Fatalf("runtime status not attached: %+v %v", got, err)
	}
	org = 65
	if _, err = svc.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("stale user org accepted: %v", err)
	}
}

func TestLocalMeshAdminCarrierUpdatePreservesConfiguredTCP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "local-mesh.json")
	before := localmesh.TCPConfig{Listen: "127.0.0.1:17700", Peers: []localmesh.TCPPeer{{Asset: 460, Address: "127.0.0.1:17701"}}}
	if err := meshsharing.SaveCarrierConfig(path, 445, before); err != nil {
		t.Fatal(err)
	}
	svc := NewLocalMeshAdminService(nil, dir, func() (int32, int32) { return 64, 445 })
	ctx := ctxWithIdentity(t, "urn:wendy:org:64:user:alice")
	if _, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Nan: proto.Bool(true), Ble: proto.Bool(true), Ethernet: proto.Bool(true), InfrastructureWifi: proto.Bool(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Nan: proto.Bool(false), Ble: proto.Bool(false), Ethernet: proto.Bool(false), InfrastructureWifi: proto.Bool(false)}); err != nil {
		t.Fatal(err)
	}
	after, err := localmesh.LoadTCPConfig(path, 445)
	if err != nil || after == nil || after.Listen != before.Listen || len(after.Peers) != 1 || after.Peers[0] != before.Peers[0] || after.NAN || after.BLE || after.Ethernet || after.InfrastructureWiFi {
		t.Fatalf("TCP configuration lost: %+v %v", after, err)
	}
}

func TestLocalMeshAdminCanDisableLastCarrierWithoutTCPConfig(t *testing.T) {
	dir := t.TempDir()
	svc := NewLocalMeshAdminService(nil, dir, func() (int32, int32) { return 64, 445 })
	ctx := ctxWithIdentity(t, "urn:wendy:org:64:user:alice")
	if _, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Nan: proto.Bool(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Ethernet: proto.Bool(true), InfrastructureWifi: proto.Bool(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Nan: proto.Bool(false), Ethernet: proto.Bool(false)}); err != nil {
		t.Fatal(err)
	}
	carriers, err := localmesh.LoadTCPConfig(filepath.Join(dir, "local-mesh.json"), 445)
	if err != nil || carriers == nil || !carriers.InfrastructureWiFi || carriers.NAN || carriers.Ethernet {
		t.Fatalf("independent Wi-Fi setting lost: %+v %v", carriers, err)
	}
	if _, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{InfrastructureWifi: proto.Bool(false)}); err != nil {
		t.Fatal(err)
	}
	carriers, err = localmesh.LoadTCPConfig(filepath.Join(dir, "local-mesh.json"), 445)
	if err != nil || carriers != nil {
		t.Fatalf("carrier-only disabled state should remove opt-in file: %+v %v", carriers, err)
	}
}
