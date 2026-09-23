package litevideo

import (
	"errors"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

func TestFirstVideoChannelCountsOnlyCameras(t *testing.T) {
	m := manifestOf(
		nonCamera(1, "mic0", sensorlinkpb.SensorDescriptor_MICROPHONE),
		camera(7, "front", sensorlinkpb.VideoFormat_MJPEG),
		nonCamera(2, "imu", sensorlinkpb.SensorDescriptor_SENSOR),
		camera(9, "rear", sensorlinkpb.VideoFormat_H264),
	)
	desc, cameras, err := FirstVideoChannel(m)
	if err != nil {
		t.Fatalf("FirstVideoChannel: %v", err)
	}
	if desc.GetChannelId() != 7 {
		t.Errorf("channel = %d, want 7", desc.GetChannelId())
	}
	if cameras != 2 {
		t.Errorf("cameras = %d, want 2 — the microphone and the IMU are not cameras", cameras)
	}
}

// An empty manifest and a manifest full of non-cameras are different faults —
// firmware that published nothing versus firmware configured without a camera —
// and an operator reading the error has to be able to tell them apart.
func TestNoCameraErrorDistinguishesAnEmptyManifest(t *testing.T) {
	_, _, err := FirstVideoChannel(manifestOf())
	if err == nil {
		t.Fatal("FirstVideoChannel succeeded on an empty manifest")
	}
	var noCam *NoCameraError
	if !errors.As(err, &noCam) {
		t.Fatalf("error = %v, want a *NoCameraError", err)
	}
	if got, want := err.Error(), "publishes no sensors at all"; !strings.Contains(got, want) {
		t.Errorf("error = %q, want it to say %q", got, want)
	}

	_, _, err = FirstVideoChannel(manifestOf(nonCamera(1, "mic0", sensorlinkpb.SensorDescriptor_MICROPHONE)))
	if got, want := err.Error(), `microphone "mic0"`; !strings.Contains(got, want) {
		t.Errorf("error = %q, want it to name %s", got, want)
	}
}
