package commands

import (
	"errors"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func ipCam(id uint32, name, addr string) *agentpb.VideoDevice {
	return &agentpb.VideoDevice{
		Id:             id,
		Name:           name,
		Address:        addr,
		Transport:      agentpb.VideoTransport_VIDEO_TRANSPORT_IP,
		HasCredentials: true,
		Online:         true,
	}
}

func usbCam(id uint32, name string) *agentpb.VideoDevice {
	return &agentpb.VideoDevice{
		Id:        id,
		Name:      name,
		Path:      "/dev/video0",
		Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_USB,
	}
}

func rosCam(id uint32, topic string) *agentpb.VideoDevice {
	return &agentpb.VideoDevice{Id: id, Name: "ROS 2 " + topic, Topic: topic, Path: "/dev/video128", Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_ROS2, Online: true}
}

// An explicit --id must never open the picker, even with several cameras.
func TestResolveCameraIDExplicitWins(t *testing.T) {
	devices := []*agentpb.VideoDevice{usbCam(0, "webcam"), ipCam(200, "RLC-520A", "10.98.0.50")}
	got, err := resolveCameraID(devices, 200, true, func([]*agentpb.VideoDevice) (uint32, error) {
		t.Fatal("picker opened despite an explicit id")
		return 0, nil
	})
	if err != nil {
		t.Fatalf("resolveCameraID: %v", err)
	}
	if got != 200 {
		t.Fatalf("id = %d, want 200", got)
	}
}

// Explicit --id 0 is a real choice, not an absent flag, so it must be honoured
// rather than triggering the picker.
func TestResolveCameraIDExplicitZero(t *testing.T) {
	devices := []*agentpb.VideoDevice{usbCam(0, "webcam"), ipCam(200, "RLC-520A", "10.98.0.50")}
	got, err := resolveCameraID(devices, 0, true, func([]*agentpb.VideoDevice) (uint32, error) {
		t.Fatal("picker opened for an explicit --id 0")
		return 0, nil
	})
	if err != nil {
		t.Fatalf("resolveCameraID: %v", err)
	}
	if got != 0 {
		t.Fatalf("id = %d, want 0", got)
	}
}

// A single camera is unambiguous, so it is selected without a prompt.
func TestResolveCameraIDSingleAutoSelects(t *testing.T) {
	got, err := resolveCameraID([]*agentpb.VideoDevice{ipCam(203, "RLC-520A", "10.98.0.50")}, 0, false,
		func([]*agentpb.VideoDevice) (uint32, error) {
			t.Fatal("picker opened for a single camera")
			return 0, nil
		})
	if err != nil {
		t.Fatalf("resolveCameraID: %v", err)
	}
	if got != 203 {
		t.Fatalf("id = %d, want 203", got)
	}
}

// This is the behaviour change for USB and CSI too: today view defaults to id 0
// with no hint that other cameras exist.
func TestResolveCameraIDMultiplePrompts(t *testing.T) {
	devices := []*agentpb.VideoDevice{usbCam(0, "webcam"), ipCam(200, "RLC-520A", "10.98.0.50")}
	called := false
	got, err := resolveCameraID(devices, 0, false, func(candidates []*agentpb.VideoDevice) (uint32, error) {
		called = true
		if len(candidates) != 2 {
			t.Fatalf("picker got %d candidates, want 2", len(candidates))
		}
		return candidates[1].GetId(), nil
	})
	if err != nil {
		t.Fatalf("resolveCameraID: %v", err)
	}
	if !called {
		t.Fatal("picker was not opened for multiple cameras")
	}
	if got != 200 {
		t.Fatalf("id = %d, want the picked camera", got)
	}
}

func TestResolveCameraIDNoCameras(t *testing.T) {
	if _, err := resolveCameraID(nil, 0, false, nil); !errors.Is(err, errNoCameras) {
		t.Fatalf("err = %v, want errNoCameras", err)
	}
}

// A cancelled picker propagates rather than falling back to camera 0.
func TestResolveCameraIDPickerError(t *testing.T) {
	devices := []*agentpb.VideoDevice{usbCam(0, "webcam"), ipCam(200, "RLC-520A", "10.98.0.50")}
	sentinel := errors.New("cancelled")
	if _, err := resolveCameraID(devices, 0, false, func([]*agentpb.VideoDevice) (uint32, error) {
		return 0, sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the picker error", err)
	}
}

// Without a picker (non-interactive) several cameras must be an actionable error
// rather than a silent guess.
func TestResolveCameraIDNonInteractiveMultiple(t *testing.T) {
	devices := []*agentpb.VideoDevice{usbCam(0, "webcam"), ipCam(200, "RLC-520A", "10.98.0.50")}
	_, err := resolveCameraID(devices, 0, false, nil)
	if err == nil {
		t.Fatal("expected an error with several cameras and no picker")
	}
}

// The rows a user reads must carry the address for network cameras and the node
// path for local ones.
func TestCameraPickerColumns(t *testing.T) {
	devices := []*agentpb.VideoDevice{usbCam(0, "webcam"), ipCam(200, "RLC-520A", "10.98.0.50")}
	items := cameraPickerItems(devices)
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	cols := cameraPickerColumns()

	var where, kinds []string
	for _, item := range items {
		for _, col := range cols {
			switch col.Title {
			case "Where":
				where = append(where, col.Value(item))
			case "Type":
				kinds = append(kinds, col.Value(item))
			}
		}
	}
	if len(where) != 2 || where[0] != "/dev/video0" || where[1] != "10.98.0.50" {
		t.Fatalf("Where column = %v", where)
	}
	if len(kinds) != 2 || kinds[0] != "usb" || kinds[1] != "ip" {
		t.Fatalf("Type column = %v", kinds)
	}
}

func TestCameraStatus(t *testing.T) {
	if got := cameraStatus(usbCam(0, "webcam"), nil); got != "ready" {
		t.Fatalf("local camera status = %q, want ready", got)
	}
	needsLogin := ipCam(200, "RLC-520A", "10.98.0.50")
	needsLogin.HasCredentials = false
	if got := cameraStatus(needsLogin, nil); got != "needs login" {
		t.Fatalf("status = %q, want needs login", got)
	}
	offline := ipCam(200, "RLC-520A", "10.98.0.50")
	offline.Online = false
	if got := cameraStatus(offline, nil); got != "offline" {
		t.Fatalf("status = %q, want offline", got)
	}
	if got := cameraStatus(ipCam(200, "RLC-520A", "10.98.0.50"), nil); got != "ready" {
		t.Fatalf("status = %q, want ready", got)
	}
}

func TestROS2CameraColumns(t *testing.T) {
	cam := rosCam(128, "/front_camera/image/compressed")
	if got := transportLabel(cam.GetTransport()); got != "ros2" {
		t.Fatalf("transport = %q", got)
	}
	if got := cameraWhere(cam); got != "/front_camera/image/compressed" {
		t.Fatalf("where = %q", got)
	}
	if got := cameraStatus(cam, nil); got != "ready" {
		t.Fatalf("status = %q", got)
	}
}

func TestTransportLabelIP(t *testing.T) {
	if got := transportLabel(agentpb.VideoTransport_VIDEO_TRANSPORT_IP); got != "ip" {
		t.Fatalf("transportLabel(IP) = %q, want ip", got)
	}
}
