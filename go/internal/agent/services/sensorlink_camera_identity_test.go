package services

import (
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSensorCameraIdentityFollowsSourceInsteadOfNode(t *testing.T) {
	identities := map[uint32]string{96: "sensorlink:source-a:0", 97: "sensorlink:source-b:0"}
	svc := &VideoService{}
	svc.SetSensorCameraIdentities(func() map[uint32]string { return identities })
	req := &agentpb.StreamVideoRequest{DeviceId: 96, StableId: "sensorlink:source-a:0"}
	got, err := svc.streamDeviceID(req)
	if err != nil || got != 96 {
		t.Fatalf("first connection: id=%d err=%v", got, err)
	}
	identities = map[uint32]string{96: "sensorlink:source-b:0", 97: "sensorlink:source-a:0"}
	got, err = svc.streamDeviceID(req)
	if err != nil || got != 97 {
		t.Fatalf("after reordered reconnect: id=%d err=%v", got, err)
	}
	delete(identities, 97)
	if _, err = svc.streamDeviceID(req); status.Code(err) != codes.NotFound {
		t.Fatalf("missing source must not fall back to device_id: %v", err)
	}
	identities = map[uint32]string{96: req.StableId, 97: req.StableId}
	if _, err = svc.streamDeviceID(req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ambiguous identity: %v", err)
	}
}

func TestNetworkCameraIdentityDoesNotFollowReusedNode(t *testing.T) {
	registry := ipcam.NewRegistry(filepath.Join(t.TempDir(), "cameras.json"))
	first, err := registry.Upsert(ipcam.Camera{MAC: "aa:bb:cc:dd:ee:01"})
	if err != nil {
		t.Fatal(err)
	}
	svc := &VideoService{registry: registry}
	listed := svc.listIPCameras()
	if len(listed) != 1 || listed[0].StableId != "ip-mac:"+first.MAC {
		t.Fatalf("identity missing: %v", listed)
	}
	req := &agentpb.StreamVideoRequest{DeviceId: first.ID, StableId: listed[0].StableId}
	if got, err := svc.streamDeviceID(req); err != nil || got != first.ID {
		t.Fatalf("resolve: %d %v", got, err)
	}
	registry.Forget(first.ID)
	second, err := registry.Upsert(ipcam.Camera{MAC: "aa:bb:cc:dd:ee:02"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatal("expected reused node for test")
	}
	if _, err := svc.streamDeviceID(req); status.Code(err) != codes.NotFound {
		t.Fatalf("must not open replacement camera: %v", err)
	}
}
