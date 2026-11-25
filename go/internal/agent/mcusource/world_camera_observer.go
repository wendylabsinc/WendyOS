package mcusource

import (
	"context"

	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
)

// CameraFrameObserver observes a native stream without opening another source
// connection. The factory is called once after a stream opens. Its closer is
// called when that stream ends, including cancellation and camera write errors.
// Frame bytes belong to the transport: an observer retaining them must copy.
// Callbacks must be bounded and must not wait for app consumers.
type CameraFrameObserver func(context.Context, SensorPairing) (
	observe func(*sensorlink.SensorFrame, ros2camera.Frame), close func())

// SetCameraFrameObserver is a private optional host hook. Install it once at
// startup, before pairings run; nil preserves the existing native path.
// wants permits an exact source/channel to be consumed without a V4L2 node.
// Without both callbacks, node failures keep the native skip behavior.
func (s *Supervisor) SetCameraFrameObserver(observer CameraFrameObserver, wants func(SensorPairing, uint32) bool) {
	s.cameraObserver = observer
	s.cameraObserverWants = wants
}

func (s *Supervisor) wantsCameraObservation(pairing SensorPairing, channel uint32) bool {
	return s.cameraObserver != nil && s.cameraObserverWants != nil && s.cameraObserverWants(pairing, channel)
}
