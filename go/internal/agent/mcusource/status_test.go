package mcusource

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"testing"
	"time"
)

type statusTransport struct {
	frames chan *sensorlinkpb.SensorFrame
}

func (*statusTransport) Close() error { return nil }
func (*statusTransport) FetchManifest(context.Context) (*sensorlinkpb.SensorManifest, error) {
	return &sensorlinkpb.SensorManifest{DeviceAssetId: 1, Sensors: []*sensorlinkpb.SensorDescriptor{{ChannelId: 1, Kind: sensorlinkpb.SensorDescriptor_CAMERA}}}, nil
}
func (t *statusTransport) Stream(context.Context, []uint32) (<-chan *sensorlinkpb.SensorFrame, func() error, error) {
	return t.frames, func() error { return nil }, nil
}

type statusLoopback struct{}

func (statusLoopback) RemoveCamera(uint32) {}

func (statusLoopback) EnsureNode(context.Context, uint32, string) error { return nil }
func (statusLoopback) NodePath(uint32) (string, bool)                   { return "camera", true }

type statusWriter struct{}

func (statusWriter) WriteFrame(ros2camera.Frame) error { return nil }
func (statusWriter) Close() error                      { return nil }

func TestConnectedOnlyWhileFramesAreDelivered(t *testing.T) {
	tr := &statusTransport{frames: make(chan *sensorlinkpb.SensorFrame)}
	s := NewSupervisor(zap.NewNop(), statusLoopback{}, func(SensorPairing, string) (SensorTransport, error) { return tr, nil }, func(string) ros2camera.CameraWriter { return statusWriter{} }, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.streamOnce(context.Background(), SensorPairing{SourceAssetID: 1}, "source")
	}()
	if s.IsConnected(1) {
		t.Fatal("connected before receiving frames")
	}
	for i := 0; i < 2; i++ {
		select {
		case tr.frames <- &sensorlinkpb.SensorFrame{ChannelId: 1}:
		case <-time.After(time.Second):
			t.Fatal("stream not consuming frames")
		}
	}
	// Receiving the second frame means the first write and status update finished.
	if !s.IsConnected(1) {
		t.Fatal("disconnected while delivering frames")
	}
	close(tr.frames)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream not stopped")
	}
	if s.IsConnected(1) {
		t.Fatal("still connected after stream ended")
	}
}
