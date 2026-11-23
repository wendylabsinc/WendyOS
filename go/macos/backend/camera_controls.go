package backend

import (
	"errors"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CameraControls uses the selected session and inventory identity so a stale
// settings sheet cannot address a camera on a newly selected device.
func (b *Backend) CameraControls(session uint64, camera uint32, stableID string, name string, value int32) ([]*pb.CameraControl, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return nil, errors.New("camera controls require a device session")
	}
	if err := b.validSession(session); err != nil {
		return nil, err
	}
	if b.lite != nil {
		if name != "" {
			return nil, errors.New("this device does not expose adjustable camera controls")
		}
		return nil, nil
	}
	device := b.cameraDevices[camera]
	if device == nil || (stableID != "" && device.GetStableId() != stableID) {
		return nil, errors.New("camera identity changed; refresh cameras")
	}
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	if name != "" {
		available, err := conn.VideoService.GetCameraControls(ctx, &pb.GetCameraControlsRequest{DeviceId: camera})
		if err != nil {
			return nil, err
		}
		valid := false
		for _, c := range available.GetControls() {
			step := max(int64(1), int64(c.Step))
			if c.Name == name && c.Mutable && value >= c.Minimum && value <= c.Maximum && (int64(value)-int64(c.Minimum))%step == 0 {
				valid = true
				break
			}
		}
		if !valid {
			return nil, errors.New("camera control is unavailable or value is outside its supported range")
		}
		_, err = conn.VideoService.SetCameraControls(ctx, &pb.SetCameraControlsRequest{DeviceId: camera, Controls: []*pb.CameraControlSetting{{Name: name, Value: value}}})
		if err != nil {
			return nil, err
		}
	}
	response, err := conn.VideoService.GetCameraControls(ctx, &pb.GetCameraControlsRequest{DeviceId: camera})
	if name == "" && status.Code(err) == codes.Unimplemented {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return response.GetControls(), nil
}
