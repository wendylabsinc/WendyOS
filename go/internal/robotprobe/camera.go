package robotprobe

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
)

// CameraDevice is one camera the agent knows about, whatever it is attached by. The
// agent already abstracts USB, CSI, network and ROS 2 cameras behind one enumeration,
// which is why this probe — and not the DDS one — is the general camera path: a robot
// with no ROS installed still has cameras, and they are still worth reporting.
type CameraDevice struct {
	StableID  string
	Name      string
	Path      string
	Model     string
	Driver    string
	Transport string
	Topic     string
	Online    bool
}

// CameraFrame is one delivered frame, stripped to what inspection measures. The pixels
// are deliberately not carried.
type CameraFrame struct {
	Width       uint32
	Height      uint32
	Fourcc      string
	Codec       string
	TimestampNs uint64
	// ReceivedAt is when this side saw the frame, which is what frame age is measured
	// against.
	ReceivedAt time.Time
}

// CameraSource enumerates and samples cameras through the agent.
type CameraSource interface {
	Cameras(ctx context.Context) ([]CameraDevice, error)
	// SampleCamera collects frames from one camera for at most window, stopping early
	// at maxFrames. Frames are capped rather than unbounded because this may run over
	// a cloud tunnel, where an uncompressed stream is expensive.
	SampleCamera(ctx context.Context, stableID string, window time.Duration, maxFrames int) ([]CameraFrame, error)
}

// ErrDeviceBusy reports that something else holds the camera. The transport recognises
// this case and wraps it, because "another app owns the device" is a different answer
// from "the device would not start", and only the first one names a cause the operator
// can act on. Everything else stays an ordinary failure rather than being guessed at.
var ErrDeviceBusy = errors.New("camera is held by another application")

// cameraSampleFrames bounds how many frames are pulled per camera. Enough to separate a
// five-frames-per-second stream from a thirty without moving a video file across a
// tunnel.
const cameraSampleFrames = 15

// Camera reports what cameras a robot has and what they actually deliver, through the
// agent rather than through DDS.
//
// This is the transport-independent camera probe: it answers for a USB webcam on an arm,
// a CSI module on a Jetson, a network camera and a ROS 2 topic alike. The DDS probes
// remain useful for what only exists on a ROS graph — a camera's calibrated intrinsics —
// but they cannot see a camera that no ROS node is publishing.
type Camera struct {
	// Window is how long to sample each camera for.
	Window time.Duration
}

func (Camera) ID() string                { return "camera" }
func (Camera) Class() robotinspect.Class { return robotinspect.ClassPassive }
func (Camera) Requires() []robotinspect.Requirement {
	return []robotinspect.Requirement{robotinspect.RequirementCameraTransport}
}

// Provides cannot be enumerated before the cameras are known, so it promises the count,
// which is always answerable — including when the answer is none.
func (Camera) Provides() []string { return []string{"camera.count"} }

func (p Camera) Observe(ctx context.Context, env *robotinspect.Env) ([]robotinspect.Property, error) {
	handle, ok := env.Handle(robotinspect.RequirementCameraTransport)
	if !ok {
		return nil, fmt.Errorf("camera: no camera handle in the environment")
	}
	source, ok := handle.(CameraSource)
	if !ok {
		return nil, fmt.Errorf("camera: camera handle is %T, not a CameraSource", handle)
	}

	cameras, err := source.Cameras(ctx)
	if err != nil {
		return nil, fmt.Errorf("camera: enumerating: %w", err)
	}

	origin := "agent:video_devices"
	properties := []robotinspect.Property{}
	if count, ok := declared(p.ID(), origin, "camera.count",
		robotinspect.MustQuantity(float64(len(cameras)), robotinspect.Count)); ok {
		properties = append(properties, count)
	}
	if len(cameras) == 0 {
		return properties, nil
	}

	window := p.Window
	if window <= 0 {
		window = DefaultStreamWindow
	}

	// Sorted so two passes over the same robot render identically.
	sort.Slice(cameras, func(i, j int) bool { return cameraKey(cameras[i]) < cameraKey(cameras[j]) })

	var failures error
	for _, camera := range cameras {
		found, err := p.observeCamera(ctx, source, camera, window)
		properties = append(properties, found...)
		if err != nil {
			failures = joinErrors(failures, err)
		}
	}
	return properties, failures
}

func (p Camera) observeCamera(ctx context.Context, source CameraSource, camera CameraDevice, window time.Duration) ([]robotinspect.Property, error) {
	key := cameraKey(camera)
	prefix := "camera." + key
	origin := "agent:video_devices"

	properties := []robotinspect.Property{
		textProperty(p.ID(), origin, prefix+".transport", camera.Transport),
	}
	for id, value := range map[string]string{
		".model":  camera.Model,
		".driver": camera.Driver,
		".path":   camera.Path,
		".topic":  camera.Topic,
		// Recorded so two documents can be aligned by a handle that survives a
		// reboot, even though the identifier itself is the readable path.
		".stable_id": camera.StableID,
	} {
		if value != "" {
			properties = append(properties, textProperty(p.ID(), origin, prefix+id, value))
		}
	}

	// A camera the agent lists but reports offline is the most useful thing here, and
	// it is not something to measure around.
	if !camera.Online {
		unknown := robotinspect.NewUnknown(robotinspect.ReasonSourceAbsent,
			fmt.Sprintf("the agent lists %s but reports it offline", key))
		return append(properties,
			robotinspect.Property{ID: prefix + ".resolution", Unknown: &unknown},
			robotinspect.Property{ID: prefix + ".rate", Unknown: &unknown}), nil
	}

	started := time.Now()
	frames, err := source.SampleCamera(ctx, camera.StableID, window, cameraSampleFrames)
	elapsed := time.Since(started)
	if elapsed > window {
		elapsed = window
	}
	if err != nil {
		// Either way the camera's inventory survives and only the measurements are
		// unknown; what differs is whether we can name the cause.
		reason := robotinspect.ReasonProbeFailed
		if errors.Is(err, ErrDeviceBusy) {
			reason = robotinspect.ReasonHeldByOther
		}
		unknown := robotinspect.NewUnknown(reason, err.Error())
		return append(properties,
			robotinspect.Property{ID: prefix + ".resolution", Unknown: &unknown},
			robotinspect.Property{ID: prefix + ".rate", Unknown: &unknown}), nil
	}
	if len(frames) == 0 {
		unknown := robotinspect.NewUnknown(robotinspect.ReasonSourceAbsent,
			fmt.Sprintf("no frames from %s in %s", key, window))
		return append(properties,
			robotinspect.Property{ID: prefix + ".resolution", Unknown: &unknown},
			robotinspect.Property{ID: prefix + ".rate", Unknown: &unknown}), nil
	}

	source_ := robotinspect.Source{Probe: p.ID(), Origin: "agent:stream_video:" + key}
	sampling := robotinspect.WithSampling(elapsed, len(frames))
	conditions := map[string]string{"codec": frames[0].Codec}
	if frames[0].Fourcc != "" {
		conditions["fourcc"] = frames[0].Fourcc
	}

	// Rate, with the same honesty rule as the DDS sampler: too short a window cannot
	// support a rate, and saying so beats reporting megahertz.
	if elapsed < minRateWindow {
		unknown := robotinspect.NewUnknown(robotinspect.ReasonWindowTooShort,
			fmt.Sprintf("sampling %s ended after %s of %s", key, elapsed, window))
		properties = append(properties, robotinspect.Property{ID: prefix + ".rate", Unknown: &unknown})
	} else {
		rate, err := robotinspect.NewQuantity(float64(len(frames))/elapsed.Seconds(), robotinspect.Hertz)
		if err != nil {
			return properties, err
		}
		observation, err := robotinspect.NewObservation(rate, robotinspect.Measured, source_,
			sampling, robotinspect.WithConditions(conditions))
		if err != nil {
			return properties, err
		}
		properties = append(properties, robotinspect.Property{
			ID: prefix + ".rate", Observations: []robotinspect.Observation{observation},
		})
	}

	// Resolution, whole and textual, when the transport reports it. An encoded stream
	// often does not, and guessing from a codec is not measuring.
	resolutions := map[string]struct{}{}
	for _, frame := range frames {
		if frame.Width > 0 && frame.Height > 0 {
			resolutions[fmt.Sprintf("%dx%d", frame.Width, frame.Height)] = struct{}{}
		}
	}
	switch {
	case len(resolutions) == 0:
		unknown := robotinspect.NewUnknown(robotinspect.ReasonSourceAbsent,
			fmt.Sprintf("%s delivered %s frames that carry no frame geometry", key, frames[0].Codec))
		properties = append(properties, robotinspect.Property{ID: prefix + ".resolution", Unknown: &unknown})
	default:
		names := sortedKeys(resolutions)
		if len(names) > 1 {
			conditions["resolutions"] = strings.Join(names, ",")
		}
		observation, err := robotinspect.NewTextObservation(names[0], robotinspect.Measured, source_,
			sampling, robotinspect.WithConditions(conditions))
		if err != nil {
			return properties, err
		}
		properties = append(properties, robotinspect.Property{
			ID: prefix + ".resolution", Observations: []robotinspect.Observation{observation},
		})
	}

	// Frame age: how stale a frame is by the time a consumer has it. A rate can look
	// healthy while every frame arrives late, which is a different fault.
	if age, ok := medianFrameAge(frames); ok {
		quantity, err := robotinspect.NewQuantity(float64(age.Microseconds())/1000, robotinspect.Milliseconds)
		if err != nil {
			return properties, err
		}
		observation, err := robotinspect.NewObservation(quantity, robotinspect.Measured, source_,
			sampling, robotinspect.WithConditions(conditions))
		if err != nil {
			return properties, err
		}
		properties = append(properties, robotinspect.Property{
			ID: prefix + ".frame_age", Observations: []robotinspect.Observation{observation},
		})
	}
	return properties, nil
}

// medianFrameAge is the median gap between a frame's capture stamp and its arrival. The
// median rather than the mean, because one stalled frame should not define the stream.
// Frames with no stamp are skipped rather than counted as instant.
func medianFrameAge(frames []CameraFrame) (time.Duration, bool) {
	var ages []time.Duration
	for _, frame := range frames {
		if frame.TimestampNs == 0 || frame.ReceivedAt.IsZero() {
			continue
		}
		age := frame.ReceivedAt.Sub(time.Unix(0, int64(frame.TimestampNs)))
		if age < 0 {
			// A capture stamp from a clock ahead of ours says more about clock drift
			// than about latency, and the clock probe already reports that.
			continue
		}
		ages = append(ages, age)
	}
	if len(ages) == 0 {
		return 0, false
	}
	sort.Slice(ages, func(i, j int) bool { return ages[i] < ages[j] })
	return ages[len(ages)/2], true
}

// cameraKey identifies a camera in a property identifier.
//
// The device path's last segment is used, not the agent's stable id. The stable id is
// the more durable handle, but on a real RealSense it reads
// "by-id:usb-Intel_R__RealSense_TM__Depth_Camera_435i_Intel_R__RealSense_TM__Depth_Camera_435i-video-index0",
// which makes both the report and every diff unreadable. The stable id is recorded as a
// property of its own instead, so two documents can still be aligned by it — and if the
// paths have shuffled between boots, that row disagrees and says so.
func cameraKey(camera CameraDevice) string {
	if camera.Path != "" {
		segments := strings.Split(strings.Trim(camera.Path, "/"), "/")
		return sanitiseKey(segments[len(segments)-1])
	}
	for _, candidate := range []string{camera.Name, camera.Model, camera.StableID} {
		if candidate != "" {
			return sanitiseKey(candidate)
		}
	}
	return "unknown"
}

// sanitiseKey makes a device string safe to embed in a dotted identifier, since the
// identifier is the contract two documents are diffed by.
func sanitiseKey(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.Trim(value, "/")) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	key := strings.Trim(b.String(), "_")
	if key == "" {
		return "unknown"
	}
	return key
}
