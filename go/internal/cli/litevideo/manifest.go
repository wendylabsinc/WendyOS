package litevideo

import (
	"fmt"
	"strings"

	"github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

// NoCameraError reports that a device's sensor manifest publishes no camera.
// It carries the manifest so the message can name what the device DOES publish:
// the usual cause is firmware configured with only an IMU or a microphone, and
// a bare "no camera" leaves the reader unable to tell that apart from a
// manifest that failed to build at all.
type NoCameraError struct {
	Manifest *sensorlinkpb.SensorManifest
}

func (e *NoCameraError) Error() string {
	others := describeSensors(e.Manifest)
	if others == "" {
		return "device publishes no sensors at all; its firmware has no sensor-link sources configured"
	}
	return "device has no camera; its sensor manifest lists " + others
}

// FirstVideoChannel returns the first camera channel in the manifest and how
// many cameras it listed.
//
// First, not best: the order is the device's own, and a viewer has no ranking
// to apply to it. The count comes back so the caller can say which one it took
// when there was a choice, rather than making that choice invisibly.
func FirstVideoChannel(m *sensorlinkpb.SensorManifest) (*sensorlinkpb.SensorDescriptor, int, error) {
	var first *sensorlinkpb.SensorDescriptor
	cameras := 0
	for _, d := range m.GetSensors() {
		if d.GetKind() != sensorlinkpb.SensorDescriptor_CAMERA {
			continue
		}
		cameras++
		if first == nil {
			first = d
		}
	}
	if first == nil {
		return nil, 0, &NoCameraError{Manifest: m}
	}
	return first, cameras, nil
}

// describeSensors renders a manifest's channels as "microphone "mic0", sensor
// "imu"", for an error that has to explain what the device offered instead of a
// camera.
func describeSensors(m *sensorlinkpb.SensorManifest) string {
	var parts []string
	for _, d := range m.GetSensors() {
		parts = append(parts, fmt.Sprintf("%s %q", sensorKindName(d.GetKind()), d.GetName()))
	}
	return strings.Join(parts, ", ")
}

func sensorKindName(k sensorlinkpb.SensorDescriptor_Kind) string {
	switch k {
	case sensorlinkpb.SensorDescriptor_CAMERA:
		return "camera"
	case sensorlinkpb.SensorDescriptor_MICROPHONE:
		return "microphone"
	case sensorlinkpb.SensorDescriptor_SENSOR:
		return "sensor"
	default:
		return "unknown channel"
	}
}
