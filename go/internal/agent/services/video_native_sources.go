package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type localCamera struct {
	path string
	req  *agentpb.StreamVideoRequest
	sink rawSink
	hub  derivingHub // nil when the capture does not feed a hub
	// sendDeciding returns the broadcast a native capture sends its frames
	// with. The first frame that reaches a subscriber runs decide, which makes
	// that capture's raw and pixel decision.
	sendDeciding func(decide func()) func([]byte, frameTimestamp, agentpb.VideoCodec) bool
}

// nativeCaptureSource is one way to hand on a camera's own frames whole. A
// camera type with a new native format is one more entry in
// nativeCaptureSources, and a derivedTransform if its format has none.
type nativeCaptureSource struct {
	format string
	// accept returns the capture of this camera for this request, or why this
	// source does not take it.
	accept func(s *VideoService, cam localCamera) (capture func(context.Context) error, declined string)
	// gaveWay reports that capture ended with nothing to show for this camera,
	// so the next source may have it, and why.
	gaveWay func(s *VideoService, cam localCamera, err error) (why string, ok bool)
}

var nativeCaptureSources = []nativeCaptureSource{nativeH264Source, nativeJPEGSource}

func (s *VideoService) captureNatively(ctx context.Context, cam localCamera) (whyNotNative string, err error) {
	var reasons []string
	for _, source := range nativeCaptureSources {
		capture, declined := source.accept(s, cam)
		if capture == nil {
			reasons = append(reasons, source.format+": "+declined)
			continue
		}
		err = capture(ctx)
		why, gaveWay := source.gaveWay(s, cam, err)
		if !gaveWay {
			return "", err
		}
		reasons = append(reasons, source.format+": "+why)
	}
	return strings.Join(reasons, "; "), nil
}

var nativeH264Source = nativeCaptureSource{
	format: "H.264",
	accept: func(s *VideoService, cam localCamera) (func(context.Context) error, string) {
		send := cam.sendDeciding(func() {
			cam.sink.rawNotOffered(fixedReason("camera streams H.264 natively; raw frames are not offered"))
		})
		return func(ctx context.Context) error { return s.streamV4L2Native(ctx, send, cam.path, cam.req) }, ""
	},
	gaveWay: func(_ *VideoService, _ localCamera, err error) (string, bool) {
		notSupported, ok := err.(nativeH264NotSupported)
		return notSupported.msg, ok
	},
}

// wholeJPEGCapture is the size and rate at which a camera is captured as whole
// JPEG frames for a request, or why it is not captured that way. A capture
// and a camera share asking whether its node can be filled read this one
// answer.
func (s *VideoService) wholeJPEGCapture(path string, req *agentpb.StreamVideoRequest) (width, height, framerate uint32, declined string) {
	width, height, declined = nativeJPEGCapture(path, req)
	if declined != "" {
		return 0, 0, 0, declined
	}
	if why, failed := s.derivedFailures.recent(path); failed {
		return 0, 0, 0, "deriving pictures and H.264 from this camera's frames failed recently: " + why
	}
	framerate = nativeJPEGFrameRate(path, req, width, height)
	if _, err := s.derivedLayerFor(nil, videoCodecMJPEG, path, width, height, framerate); err != nil {
		return 0, 0, 0, fmt.Sprintf("pictures and H.264 cannot be derived from its frames on this device: %v", err)
	}
	return width, height, framerate, ""
}

var nativeJPEGSource = nativeCaptureSource{
	format: "JPEG",
	accept: func(s *VideoService, cam localCamera) (func(context.Context) error, string) {
		width, height, framerate, declined := s.wholeJPEGCapture(cam.path, cam.req)
		if declined != "" {
			return nil, declined
		}
		if cam.hub == nil {
			return nil, "the capture does not feed a camera hub"
		}
		layer, err := s.derivedLayerFor(cam.hub, videoCodecMJPEG, cam.path, width, height, framerate)
		if err != nil {
			return nil, fmt.Sprintf("pictures and H.264 cannot be derived from its frames on this device: %v", err)
		}
		send := cam.sendDeciding(func() { layer.offerTo(cam.sink) })
		return func(ctx context.Context) error {
			return s.captureDeriving(ctx, cam.hub, layer, func(ctx context.Context) error {
				return s.captureNativeJPEG(ctx, send, cam.path, width, height, framerate)
			})
		}, ""
	},
	gaveWay: func(s *VideoService, cam localCamera, err error) (string, bool) {
		if notSupported, ok := err.(nativeJPEGNotSupported); ok {
			return notSupported.msg, true
		}
		var gaveUp derivedLayerGaveUp
		if !errors.As(err, &gaveUp) {
			return "", false
		}
		s.derivedFailures.remember(cam.path, gaveUp.msg)
		s.logger.Error("derived frame pipeline is unusable for this camera; GStreamer captures it instead",
			zap.String("device", cam.path), zap.Duration("retried_after", derivedLayerRetryAfter), zap.String("reason", gaveUp.msg))
		// A capture that already published whole access units ends here instead:
		// consumers read a stream's alignment off its first frame, and GStreamer's
		// chunks may not follow whole units in one stream. The next hub starts on
		// GStreamer.
		return gaveUp.msg, !gaveUp.afterPublishing
	},
}

const derivedLayerRetryAfter = 10 * time.Minute

type derivedLayerFailures struct {
	mu     sync.Mutex
	failed map[string]derivedLayerFailure
	now    func() time.Time
}

type derivedLayerFailure struct {
	why   string
	until time.Time
}

func (f *derivedLayerFailures) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

func (f *derivedLayerFailures) remember(device, why string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed == nil {
		f.failed = make(map[string]derivedLayerFailure)
	}
	f.failed[device] = derivedLayerFailure{why: why, until: f.clock().Add(derivedLayerRetryAfter)}
}

func (f *derivedLayerFailures) recent(device string) (why string, failed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	failure, known := f.failed[device]
	if !known {
		return "", false
	}
	if !f.clock().Before(failure.until) {
		delete(f.failed, device)
		return "", false
	}
	return failure.why, true
}

// derivedLayerFor builds the layer that derives from a camera's frames for hub.
// Without a hub it only answers whether such a layer can be built here.
func (s *VideoService) derivedLayerFor(hub derivingHub, format agentpb.VideoCodec, path string, width, height, framerate uint32) (*derivedLayer, error) {
	gstPath, available, err := s.gstElements()
	if err != nil {
		return nil, fmt.Errorf("GStreamer is not usable: %w", err)
	}
	cfg := derivedLayerConfig{
		logger: s.logger, device: path, format: format, gstPath: gstPath, available: available,
		width: width, height: height, framerate: framerate, grace: s.encoderGrace(),
	}
	if hub != nil {
		cfg.demand, cfg.publish = hub, hub.broadcastDerivedFrom
	}
	return newDerivedLayer(cfg)
}

// A layer that gives up ends the capture, with the layer's error: the hub's
// viewers and pixel readers would otherwise wait on frames that will not come.
func (s *VideoService) captureDeriving(ctx context.Context, hub derivingHub, layer *derivedLayer, capture func(context.Context) error) error {
	detach := hub.feedDerivedLayer(layer)
	defer detach()
	captureCtx, stopCapture := context.WithCancel(ctx)
	defer stopCapture()
	layerCtx, stopLayer := context.WithCancel(ctx)
	defer stopLayer()
	layerEnded := make(chan error, 1)
	var layerGaveUp atomic.Bool
	go func() {
		err := layer.run(layerCtx)
		if layerCtx.Err() == nil {
			layerGaveUp.Store(true)
			stopCapture()
		}
		layerEnded <- err
	}()
	err := capture(captureCtx)
	stopLayer()
	layerErr := <-layerEnded
	if layerGaveUp.Load() && ctx.Err() == nil {
		return layerErr
	}
	return err
}
