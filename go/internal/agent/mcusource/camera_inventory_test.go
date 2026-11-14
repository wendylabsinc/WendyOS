package mcusource

import (
	"context"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"testing"
)

func TestCameraInventorySurvivesReorderedConnectionsAndRetainedNodes(t *testing.T) {
	lb := &inventoryLoopback{}
	first := NewSupervisor(zap.NewNop(), lb, nil, nil, nil)
	channel := &sensorlinkpb.SensorDescriptor{ChannelId: 0, Name: "camera"}
	source := func(name string) SensorPairing {
		return SensorPairing{Name: name, SourcePrincipalURI: "spiffe://wendy.sh/tenant/test/device/" + name}
	}
	for _, name := range []string{"camera-03", "camera-01", "camera-02", "mesh-01"} {
		p := source(name)
		id, err := first.nodeIDForKey(p.Key(), 0)
		if err != nil {
			t.Fatal(err)
		}
		first.mountCamera(id, p, channel)
	}
	// The kernel loopback survives but camera-01 is absent after restart.
	next := NewSupervisor(zap.NewNop(), lb, nil, nil, nil)
	for _, name := range []string{"camera-03", "camera-02", "mesh-01"} {
		p := source(name)
		id, err := next.nodeIDForKey(p.Key(), 0)
		if err != nil {
			t.Fatal(err)
		}
		next.mountCamera(id, p, channel)
		next.cameraFrame(p, 0)
	}
	if first.Cameras()[97].Name != "camera-01:camera" {
		t.Fatal("bad retained-label setup")
	}
	got := next.Cameras()
	if got[97].Name != "camera-02:camera" || got[97].StableID != "sensorlink:"+source("camera-02").Key()+":0" || !got[97].Online {
		t.Fatalf("reordered camera inventory = %#v", got)
	}
	if got[98].Name != "mesh-01:camera" {
		t.Fatalf("reordered mesh camera = %#v", got[98])
	}
	// Disconnect retains identity and name, but not liveness.
	p := source("camera-02")
	next.camerasDisconnected(p)
	if next.Cameras()[97].Online {
		t.Fatal("disconnected source reported live")
	}
	// A reconnect with a new display name must not inherit the kernel card label.
	p.Name = "A much longer renamed camera than fits in a kernel label"
	next.mountCamera(97, p, channel)
	if next.Cameras()[97].Name != p.Name+":camera" {
		t.Fatal("name stale or truncated")
	}
	// Unpair can leave a busy node reserved; it must no longer be advertised.
	next.releasePairing(p)
	if _, ok := next.Cameras()[97]; ok {
		t.Fatal("unpaired busy node still advertised")
	}
	if _, ok := next.nodeIDs[p.Key()+":0"]; !ok {
		t.Fatal("busy node reservation lost")
	}
}

func TestCameraInventoryTracksChannelsNotOnlyPairing(t *testing.T) {
	s := NewSupervisor(zap.NewNop(), &inventoryLoopback{}, nil, nil, nil)
	p := SensorPairing{SourceAssetID: 1, Name: "two cameras"}
	for channel := uint32(0); channel < 2; channel++ {
		id, _ := s.nodeIDForKey(p.Key(), channel)
		s.mountCamera(id, p, &sensorlinkpb.SensorDescriptor{ChannelId: channel, Name: "camera"})
	}
	s.cameraFrame(p, 0)
	got := s.Cameras()
	if !got[96].Online || got[97].Online {
		t.Fatalf("frame on one camera made both healthy: %#v", got)
	}
}

type inventoryLoopback struct{}

func (*inventoryLoopback) EnsureNode(context.Context, uint32, string) error { return nil }
func (*inventoryLoopback) NodePath(uint32) (string, bool)                   { return "/dev/video-fake", true }
func (*inventoryLoopback) RemoveCamera(uint32)                              {}
