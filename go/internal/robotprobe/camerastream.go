package robotprobe

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
)

// CameraStream measures what a camera actually delivers, as opposed to what it claims.
// It samples the image topic for a window and reports the resolution of the frames that
// arrived and the rate they arrived at.
//
// This is the half that turns an inventory into a finding. A camera's CameraInfo gives
// the resolution its intrinsics were calibrated at; this gives the resolution of the
// frames a consumer receives. On the robot this whole effort started from, those were
// 640x480 and 848x480, and the field of view was quoted from one against the other.
type CameraStream struct {
	// Topics are the image topics to sample.
	Topics []string
	// Window is how long to sample for. A rate measured over too short a window is
	// noise, so this is the figure that appears in the report as the sampling window.
	Window time.Duration
}

// minRateWindow is the shortest span a frame rate can honestly be derived from.
const minRateWindow = 250 * time.Millisecond

// DefaultStreamWindow is long enough to separate a five-frames-per-second stream from a
// thirty without keeping an operator waiting.
const DefaultStreamWindow = 5 * time.Second

func (CameraStream) ID() string { return "stream-sample" }

// Class is passive: subscribing to a topic a camera already publishes cannot move
// anything, and nothing here writes to the robot.
func (CameraStream) Class() robotinspect.Class { return robotinspect.ClassPassive }

func (CameraStream) Requires() []robotinspect.Requirement {
	return []robotinspect.Requirement{robotinspect.RequirementDDSDomain}
}

func (p CameraStream) Provides() []string {
	ids := make([]string, 0, len(p.Topics)*2)
	for _, topic := range p.Topics {
		stream := streamName(topic)
		ids = append(ids,
			fmt.Sprintf("camera.%s.resolution.width", stream),
			fmt.Sprintf("camera.%s.rate", stream))
	}
	return ids
}

func (p CameraStream) Observe(ctx context.Context, env *robotinspect.Env) ([]robotinspect.Property, error) {
	handle, ok := env.Handle(robotinspect.RequirementDDSDomain)
	if !ok {
		return nil, fmt.Errorf("stream-sample: no DDS handle in the environment")
	}
	reader, ok := handle.(TopicReader)
	if !ok {
		return nil, fmt.Errorf("stream-sample: DDS handle is %T, not a TopicReader", handle)
	}

	window := p.Window
	if window <= 0 {
		window = DefaultStreamWindow
	}

	var properties []robotinspect.Property
	var failures error
	for _, topic := range p.Topics {
		found, err := p.observeTopic(ctx, reader, topic, window)
		properties = append(properties, found...)
		if err != nil {
			failures = joinErrors(failures, err)
		}
	}
	return properties, failures
}

func (p CameraStream) observeTopic(ctx context.Context, reader TopicReader, topic string, window time.Duration) ([]robotinspect.Property, error) {
	stream := streamName(topic)
	started := time.Now()
	payloads, err := reader.Sample(ctx, topic, rosmsg.TypeImage, window, 0)
	elapsed := time.Since(started)
	if err != nil {
		return nil, fmt.Errorf("stream-sample: sampling %s: %w", topic, err)
	}
	// The window is what was asked for; elapsed is what happened. Reporting the
	// shorter of the two keeps a rate honest when sampling was cut short.
	if elapsed > window {
		elapsed = window
	}

	source := robotinspect.Source{Probe: p.ID(), Origin: "topic:" + topic}

	if len(payloads) == 0 {
		// A camera that publishes nothing is the most important thing this probe can
		// find, and reporting zero frames per second would imply it was measured.
		unknown := robotinspect.NewUnknown(robotinspect.ReasonSourceAbsent,
			fmt.Sprintf("no frames on %s in %s", topic, window))
		return []robotinspect.Property{
			{ID: fmt.Sprintf("camera.%s.rate", stream), Unknown: &unknown},
			{ID: fmt.Sprintf("camera.%s.resolution.width", stream), Unknown: &unknown},
		}, nil
	}

	var properties []robotinspect.Property
	sampling := robotinspect.WithSampling(elapsed, len(payloads))

	// Rate is frames over the window they arrived in — but only when that window was
	// long enough to mean something. Sampling can end early on cancellation or a
	// closed participant, and a few frames divided by a near-zero elapsed time reads
	// as megahertz. Saying the window was too short is the honest answer.
	rateID := fmt.Sprintf("camera.%s.rate", stream)
	if elapsed < minRateWindow || elapsed*2 < window {
		unknown := robotinspect.NewUnknown(robotinspect.ReasonWindowTooShort,
			fmt.Sprintf("sampling %s ended after %s of %s", topic, elapsed, window))
		properties = append(properties, robotinspect.Property{ID: rateID, Unknown: &unknown})
	} else {
		rate, err := robotinspect.NewQuantity(float64(len(payloads))/elapsed.Seconds(), robotinspect.Hertz)
		if err != nil {
			return properties, err
		}
		observation, err := robotinspect.NewObservation(rate, robotinspect.Measured, source, sampling)
		if err != nil {
			return properties, err
		}
		properties = append(properties, robotinspect.Property{
			ID:           rateID,
			Observations: []robotinspect.Observation{observation},
		})
	}

	// Resolution comes from the frames themselves. A stream that changes resolution
	// mid-window is reported as such rather than averaged into a number that no frame
	// ever had.
	header, err := rosmsg.DecodeImageHeader(payloads[0])
	if err != nil {
		return properties, fmt.Errorf("stream-sample: decoding a frame from %s: %w", topic, err)
	}
	resolutions := map[string]struct{}{header.Resolution(): {}}
	for _, payload := range payloads[1:] {
		if other, err := rosmsg.DecodeImageHeader(payload); err == nil {
			resolutions[other.Resolution()] = struct{}{}
		}
	}
	conditions := map[string]string{"encoding": header.Encoding}
	if header.FrameID != "" {
		conditions["frame"] = header.FrameID
	}
	if len(resolutions) > 1 {
		conditions["resolutions"] = strings.Join(sortedKeys(resolutions), ",")
	}

	width, err := robotinspect.NewQuantity(float64(header.Width), robotinspect.Count)
	if err != nil {
		return properties, err
	}
	observation, err := robotinspect.NewObservation(width, robotinspect.Measured, source,
		sampling, robotinspect.WithConditions(conditions))
	if err != nil {
		return properties, err
	}
	return append(properties, robotinspect.Property{
		ID:           fmt.Sprintf("camera.%s.resolution.width", stream),
		Observations: []robotinspect.Observation{observation},
	}), nil
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
