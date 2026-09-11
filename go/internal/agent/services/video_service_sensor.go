package services

import (
	"context"
	"fmt"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// canonicalCameraSourceID rebuilds the identifier `wendy data sources` publishes
// for an already-resolved camera.
//
// cameraDeviceID is lossy on purpose — it exists to turn an identifier into a
// device number — so several spellings collapse onto one device: "ipcamera:0",
// "v4l2:/dev/video00" and "v4l2:/dev/video0" all yield device 0, and only IDs
// inside the IP band resolve to a network camera. Accepting an alias here would
// be wrong twice over. The episode's model-input ledger joins to the capture
// index on source_id, so a ledger line written under an alias names a source no
// index and no manifest entry can resolve, and the manifest would additionally
// report the camera as not_captured_by_this_episode while the episode was in
// fact capturing it. It is also an entitlement question: an allowlist naming
// "ipcamera:0" must not silently grant /dev/video0.
func canonicalCameraSourceID(src videoSource, devID uint32) string {
	if src.kind == sourceIP {
		return fmt.Sprintf("ipcamera:%d", devID)
	}
	return "v4l2:" + src.path
}

// resolveSensorSource resolves a sensor source identifier to its camera,
// refusing any spelling that is not the canonical identifier for that camera.
func (s *VideoService) resolveSensorSource(sourceID string) (videoSource, error) {
	devID, ok := cameraDeviceID(sourceID)
	if !ok {
		return videoSource{}, status.Errorf(codes.NotFound, "source %q is not a camera", sourceID)
	}
	src, err := s.resolveSource(devID)
	if err != nil {
		return videoSource{}, err
	}
	if canonical := canonicalCameraSourceID(src, devID); canonical != sourceID {
		return videoSource{}, status.Errorf(codes.NotFound,
			"source %q is not a camera identifier; this camera is %q", sourceID, canonical)
	}
	return src, nil
}

// SupportsSensorSource reports whether a `wendy data sources` identifier names
// a camera this service can hand to a model subscriber.
func (s *VideoService) SupportsSensorSource(sourceID string) bool {
	_, err := s.resolveSensorSource(sourceID)
	return err == nil
}

// SubscribeSensor joins the device's existing frame hub as one more subscriber.
//
// This is the whole point of routing model input through the harness: the hub
// already multiplexes one producer to sixteen consumers, so an app and the
// episode capture adapter can both consume the same camera. Nothing here opens
// a device — if no producer is running, one is started and shared; if one is
// running with different stream parameters, this joins it at the parameters it
// has rather than failing or restarting it. Subscribing never takes a device
// away from anyone. The reverse does not hold: episode capture with explicit
// campaign parameters may restart a producer this subscriber started at
// defaults (see takeOverDefaultedHub), in which case the subscription
// reattaches to the restarted stream — a restart initiated by capture is not
// the subscriber taking anything.
func (s *VideoService) SubscribeSensor(ctx context.Context, sourceID string) (sensorSubscription, error) {
	src, err := s.resolveSensorSource(sourceID)
	if err != nil {
		return nil, err
	}
	devID, _ := cameraDeviceID(sourceID)
	if src.kind == sourceIP {
		if err := s.preflightIPCamera(src.camera); err != nil {
			return nil, err
		}
	}
	// No stream parameters are asserted: a model subscriber must never take a
	// running stream away from a viewer or from episode capture. An empty
	// request also matches the parameters episode capture uses by default, so
	// the common case joins one hub rather than creating a second.
	hub, subID, frames, err := s.joinHub(ctx, src.key, &agentpb.StreamVideoRequest{DeviceId: devID})
	if err != nil {
		return nil, err
	}
	return &cameraSensorSubscription{video: s, key: src.key, devID: devID, hub: hub, subID: subID, frames: frames}, nil
}

// cameraSensorSubscription adapts one hub subscription to the sensor contract.
type cameraSensorSubscription struct {
	// video, key and devID are what reattaching after a capture takeover
	// needs: the subscription asserted no parameters, so when episode capture
	// restarts the producer at the campaign's parameters this subscriber
	// simply rejoins and gets what the producer now provides. video is nil in
	// unit tests that drive a bare hub; those never see a restarted hub.
	video  *VideoService
	key    string
	devID  uint32
	hub    *deviceHub
	subID  int
	frames chan *videoFrame
	// lastDrops is the hub's drop counter for this subscriber as of the
	// previously delivered sample, so each sample reports the drops since it.
	lastDrops uint64
	// awaitRandomAccess gates delivery after a reattach: the subscriber's old
	// stream ended with the restarted producer, and the new one must begin on
	// a random-access unit so the app decodes from a clean start rather than
	// from the middle of a group of pictures. This is the subscriber-side
	// counterpart of the rule episode capture enforces with
	// errAwaitCameraRandomAccess. Frames skipped by the gate are reported in
	// gatedSkips so the sample_id gap they leave stays explained.
	awaitRandomAccess bool
	gatedSkips        uint64
	closed            bool
}

func (c *cameraSensorSubscription) Next(ctx context.Context) (SensorSample, error) {
	for {
		select {
		case <-ctx.Done():
			return SensorSample{}, ctx.Err()
		case frame, ok := <-c.frames:
			if !ok {
				if err := c.hub.terminalErr(); err != nil {
					return SensorSample{}, err
				}
				// Episode capture restarted the producer at the campaign's
				// parameters. This subscription asserted none, so it reattaches
				// to the restarted stream: its old stream has ended and a new
				// one begins, never a mid-stream splice of different sequence
				// parameter sets into one timeline.
				if c.video != nil && c.hub.wasRestarted() {
					if err := c.reattach(ctx); err != nil {
						return SensorSample{}, err
					}
					continue
				}
				return SensorSample{}, errSensorProducerStopped
			}
			if c.awaitRandomAccess && frame.auAligned {
				if _, randomAccess := frameRandomAccess(frame); !randomAccess {
					c.gatedSkips++
					continue
				}
			}
			c.awaitRandomAccess = false
			drops := c.hub.drops(c.subID)
			delta := drops - c.lastDrops + c.gatedSkips
			c.lastDrops = drops
			c.gatedSkips = 0
			return SensorSample{
				SampleID:         frame.sampleID,
				BootNanos:        frame.receiptBootNanos,
				UncertaintyNanos: frame.receiptUncertaintyNanos,
				Payload:          frame.data,
				Encoding:         codecEncodingName(frame.codec),
				SelfContained:    frame.auAligned,
				DroppedBefore:    delta,
			}, nil
		}
	}
}

// reattach joins the replacement hub after a capture takeover, again asserting
// no parameters. The hub-side drop counter starts at zero on the new
// subscription, and delivery is gated to a random-access unit so the app's new
// stream starts decodable. Drops the old subscription accrued after the last
// delivered sample can no longer ride on a sample of their own, so they are
// carried into gatedSkips and reported on the first sample the new
// subscription delivers; the restart leaves no loss unreported.
func (c *cameraSensorSubscription) reattach(ctx context.Context) error {
	c.gatedSkips += c.hub.unsubscribe(c.subID) - c.lastDrops
	hub, subID, frames, err := c.video.joinHub(ctx, c.key, &agentpb.StreamVideoRequest{DeviceId: c.devID})
	if err != nil {
		return err
	}
	c.hub, c.subID, c.frames = hub, subID, frames
	c.lastDrops = 0
	c.awaitRandomAccess = true
	return nil
}

func (c *cameraSensorSubscription) Close() {
	if c.closed {
		return
	}
	c.closed = true
	c.hub.unsubscribe(c.subID)
}

// codecEncodingName names the payload bytes for a model subscriber. It is the
// codec, not a container: a sample is raw encoded video, and whether it is a
// whole access unit is reported separately.
func codecEncodingName(codec agentpb.VideoCodec) string {
	switch codec {
	case agentpb.VideoCodec_VIDEO_CODEC_H264:
		return "h264"
	case agentpb.VideoCodec_VIDEO_CODEC_VP8:
		return "vp8"
	default:
		return "unknown"
	}
}
