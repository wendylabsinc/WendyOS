package robotprobe

import (
	"context"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
)

// cameraInfoWindow bounds the wait for one CameraInfo message. The topic is latched in
// practice, so a message arrives immediately or the publisher is not there.
const cameraInfoWindow = 2 * time.Second

// CameraInfo reads what a camera claims about itself from its CameraInfo topic: the
// resolution its intrinsics were calibrated at, and the field of view those intrinsics
// imply on each axis.
//
// Both axes are reported as separate properties. They are not two answers to one
// question — a horizontal and a vertical field of view are both correct and always
// differ — so putting them on one property would manufacture a disagreement out of a
// correctly working camera. Keeping them apart here is what leaves the incomparable
// verdict free to mean what it says: that two sources disagree about which axis they
// are quoting.
type CameraInfo struct {
	// Topics are the CameraInfo topics to read, as discovered by the caller.
	Topics []string
}

func (CameraInfo) ID() string { return "camerainfo" }

// Class is passive: reading a latched topic cannot move anything.
func (CameraInfo) Class() robotinspect.Class { return robotinspect.ClassPassive }

func (CameraInfo) Requires() []robotinspect.Requirement {
	return []robotinspect.Requirement{robotinspect.RequirementDDSDomain}
}

// Provides lists a property per axis per stream, so a camera that turns out to be
// uncalibrated still leaves a row in the report saying so.
func (p CameraInfo) Provides() []string {
	ids := make([]string, 0, len(p.Topics)*3)
	for _, topic := range p.Topics {
		stream := streamName(topic)
		ids = append(ids,
			fmt.Sprintf("camera.%s.resolution.width", stream),
			fmt.Sprintf("camera.%s.fov.horizontal", stream),
			fmt.Sprintf("camera.%s.fov.vertical", stream))
	}
	return ids
}

func (p CameraInfo) Observe(ctx context.Context, env *robotinspect.Env) ([]robotinspect.Property, error) {
	handle, ok := env.Handle(robotinspect.RequirementDDSDomain)
	if !ok {
		return nil, fmt.Errorf("camerainfo: no DDS handle in the environment")
	}
	reader, ok := handle.(TopicReader)
	if !ok {
		return nil, fmt.Errorf("camerainfo: DDS handle is %T, not a TopicReader", handle)
	}

	var properties []robotinspect.Property
	var failures error
	for _, topic := range p.Topics {
		found, err := p.observeTopic(ctx, reader, topic)
		properties = append(properties, found...)
		if err != nil {
			failures = joinErrors(failures, err)
		}
	}
	return properties, failures
}

func (p CameraInfo) observeTopic(ctx context.Context, reader TopicReader, topic string) ([]robotinspect.Property, error) {
	stream := streamName(topic)
	payloads, err := reader.Sample(ctx, topic, rosmsg.TypeCameraInfo, cameraInfoWindow, 1)
	if err != nil {
		return nil, fmt.Errorf("camerainfo: sampling %s: %w", topic, err)
	}
	if len(payloads) == 0 {
		return unknownCamera(stream, robotinspect.ReasonSourceAbsent,
			fmt.Sprintf("nothing published on %s within %s", topic, cameraInfoWindow)), nil
	}

	info, err := rosmsg.DecodeCameraInfo(payloads[0])
	if err != nil {
		return unknownCamera(stream, robotinspect.ReasonProbeFailed, err.Error()),
			fmt.Errorf("camerainfo: decoding %s: %w", topic, err)
	}

	origin := "topic:" + topic
	source := robotinspect.Source{Probe: p.ID(), Origin: origin}
	properties := []robotinspect.Property{}

	// The calibrated resolution is what the camera says it was measured at. It is the
	// declared counterpart to whatever a stream sample actually delivers.
	if width, err := robotinspect.NewQuantity(float64(info.Width), robotinspect.Count); err == nil {
		if o, err := robotinspect.NewObservation(width, robotinspect.Declared, source); err == nil {
			properties = append(properties, robotinspect.Property{
				ID:           fmt.Sprintf("camera.%s.resolution.width", stream),
				Observations: []robotinspect.Observation{o},
			})
		}
	}

	horizontal, vertical, err := info.FieldOfView()
	if err != nil {
		// A camera publishing no intrinsics is a real and common state. Saying so is
		// the answer; deriving 180 degrees from a zero focal length is not.
		return append(properties, unknownCamera(stream, robotinspect.ReasonUncalibrated, err.Error())...), nil
	}

	conditions := map[string]string{"resolution": info.Resolution()}
	if info.FrameID != "" {
		conditions["frame"] = info.FrameID
	}
	for _, axis := range []struct {
		name    string
		degrees float64
	}{
		{robotinspect.AxisHorizontal, horizontal},
		{robotinspect.AxisVertical, vertical},
	} {
		// Derived, not declared: the camera publishes focal lengths, and the angle is
		// computed from them. The source names the topic it came from either way.
		quantity, err := robotinspect.NewQuantity(axis.degrees, robotinspect.Degrees,
			robotinspect.WithAxis(axis.name))
		if err != nil {
			return properties, fmt.Errorf("camerainfo: %s field of view: %w", axis.name, err)
		}
		observation, err := robotinspect.NewObservation(quantity, robotinspect.Derived, source,
			robotinspect.WithConditions(conditions))
		if err != nil {
			return properties, fmt.Errorf("camerainfo: %s field of view: %w", axis.name, err)
		}
		properties = append(properties, robotinspect.Property{
			ID:           fmt.Sprintf("camera.%s.fov.%s", stream, axis.name),
			Observations: []robotinspect.Observation{observation},
		})
	}
	return properties, nil
}

// unknownCamera leaves a row per axis rather than omitting the camera, so a report never
// falls silent about something it was asked to look at.
func unknownCamera(stream, reason, detail string) []robotinspect.Property {
	unknown := robotinspect.NewUnknown(reason, detail)
	return []robotinspect.Property{
		{ID: fmt.Sprintf("camera.%s.fov.%s", stream, robotinspect.AxisHorizontal), Unknown: &unknown},
		{ID: fmt.Sprintf("camera.%s.fov.%s", stream, robotinspect.AxisVertical), Unknown: &unknown},
	}
}

func joinErrors(existing, err error) error {
	if existing == nil {
		return err
	}
	return fmt.Errorf("%w; %w", existing, err)
}
