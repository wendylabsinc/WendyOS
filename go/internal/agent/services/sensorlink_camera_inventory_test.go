package services

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
	"time"
)

func TestSensorCameraInventoryOverridesRetainedKernelLabel(t *testing.T) {
	svc := newTestVideoService(func() ([]string, error) { return []string{"/dev/video97", "/dev/video98"}, nil }, func(string) (string, error) { return "old-camera:camera", nil })
	current := map[uint32]mcusource.Camera{97: {Name: "actual-camera:camera", StableID: "sensorlink:current:0", Online: true}}
	svc.SetSensorCameras(func() map[uint32]mcusource.Camera { return current })
	devices, err := svc.listCameras(context.Background())
	if err != nil || len(devices) != 1 {
		t.Fatalf("list = %v, %v", devices, err)
	}
	d := devices[0]
	if d.Name != current[97].Name || d.StableId != current[97].StableID || !d.Online {
		t.Fatalf("split identity metadata: %v", d)
	}
	current[97] = mcusource.Camera{Name: d.Name, StableID: d.StableId}
	devices, err = svc.listCameras(context.Background())
	if err != nil || len(devices) != 1 || devices[0].Online {
		t.Fatalf("disconnected camera: %v %v", devices, err)
	}
	sources := (&cameraDataAdapter{video: svc}).Discover(context.Background())
	if len(sources) != 1 || sources[0].ID != d.StableId || sources[0].Healthy || sources[0].UnusableReason == "" {
		t.Fatalf("data discovery: %#v", sources)
	}
}

func TestSensorDataSourceFollowsIdentityAndRefusesMissingCamera(t *testing.T) {
	svc := newTestVideoService(func() ([]string, error) { return []string{"/dev/video96", "/dev/video97"}, nil }, func(string) (string, error) { return "stale label", nil })
	current := map[uint32]mcusource.Camera{96: {Name: "one", StableID: "sensorlink:one:0", Online: true}, 97: {Name: "two", StableID: "sensorlink:two:0", Online: true}}
	svc.SetSensorCameras(func() map[uint32]mcusource.Camera { return current })
	a := &cameraDataAdapter{video: svc}
	sources := a.Discover(context.Background())
	if len(sources) != 2 || sources[0].ID != "sensorlink:one:0" || sources[0].Aliases[0] != "v4l2:/dev/video96" {
		t.Fatalf("sources=%#v", sources)
	}
	current[96], current[97] = current[97], current[96]
	id, err := svc.dataCameraDeviceID(sources[0].ID)
	if err != nil || id != 97 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	src, err := svc.resolveSensorSource(sources[0].ID)
	if err != nil || src.path != "/dev/video97" {
		t.Fatalf("inference source=%#v err=%v", src, err)
	}
	delete(current, 97)
	if _, err = svc.dataCameraDeviceID(sources[0].ID); status.Code(err) != codes.NotFound {
		t.Fatalf("missing identity fell back: %v", err)
	}
	if _, _, _, err = svc.joinDataCamera(context.Background(), sources[0].ID, "/dev/video96", 96); status.Code(err) != codes.NotFound {
		t.Fatalf("rejoin used old node: %v", err)
	}
}

func TestSensorCameraControlsRejectVirtualNode(t *testing.T) {
	svc := &VideoService{}
	if _, err := svc.GetCameraControls(context.Background(), &agentpb.GetCameraControlsRequest{DeviceId: 97}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("get virtual controls: %v", err)
	}
	if _, err := svc.SetCameraControls(context.Background(), &agentpb.SetCameraControlsRequest{DeviceId: 97}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("set virtual controls: %v", err)
	}
	if _, err := svc.ResetCameraControls(context.Background(), &agentpb.ResetCameraControlsRequest{DeviceId: 97}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("reset virtual controls: %v", err)
	}
}

func TestSensorDataSubscriptionAndStandbyUseStableIdentity(t *testing.T) {
	svc := newTestVideoService(func() ([]string, error) { return []string{"/dev/video96", "/dev/video97"}, nil }, func(string) (string, error) { return "retained label", nil })
	defer svc.Shutdown()
	fp := installFakeProducers(svc)
	current := map[uint32]mcusource.Camera{97: {StableID: "sensorlink:one:0", Name: "one", Online: true}}
	svc.SetSensorCameras(func() map[uint32]mcusource.Camera { return current })
	subscription, err := svc.SubscribeSensor(context.Background(), "sensorlink:one:0")
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	cs := subscription.(*cameraSensorSubscription)
	if cs.devID != 97 || cs.key != "/dev/video97" {
		t.Fatalf("wrong inference target: %d %s", cs.devID, cs.key)
	}
	adapter := &cameraDataAdapter{video: svc}
	sources := adapter.Discover(context.Background())
	adapter.Arm("stable-campaign", time.Second, sources)
	defer adapter.Disarm("stable-campaign")
	if fp.count() != 1 {
		t.Fatalf("standby did not share selected camera: %d producers", fp.count())
	}
	// An unpaired identity cannot reattach to the stale numeric slot.
	delete(current, 97)
	if err := cs.reattach(context.Background()); status.Code(err) != codes.NotFound {
		t.Fatalf("reattach after unpair: %v", err)
	}
	if fp.count() != 1 {
		t.Fatal("missing identity started another producer")
	}
}
