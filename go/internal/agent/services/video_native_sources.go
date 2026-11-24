package services

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// ownFrames describes what a native source hands on: the codec of the camera's
// own frames and the size and rate they are captured at.
type ownFrames struct {
	codec                    agentpb.VideoCodec
	width, height, framerate uint32
}

// nativeCaptureSource is one way to hand on a camera's own frames whole. A
// camera type with a new native format is one more entry in
// nativeCaptureSources, and a derivedTransform if its format has none.
type nativeCaptureSource struct {
	format string
	// frames answers for a request without capturing: the frames this source
	// hands on, or why it hands on none. passes says the capture then goes to
	// the next source; without it this source captures the camera all the same
	// and what it hands on is known only once it runs.
	frames func(s *VideoService, path string, req *agentpb.StreamVideoRequest) (frames ownFrames, declined string, passes bool)
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
		if why, refused := s.sourceRefusals.recent(sourceRefusalKey(cam.path, source.format)); refused {
			reasons = append(reasons, source.format+": "+why)
			continue
		}
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
		// What the camera lists is not what it delivers: the next capture and
		// the share policy go by what happened here.
		s.sourceRefusals.remember(sourceRefusalKey(cam.path, source.format), why, false)
		reasons = append(reasons, source.format+": "+why)
	}
	return strings.Join(reasons, "; "), nil
}

// ownFramesFor asks the sources in the order captureNatively tries them.
// native says one of them captures the camera, described or not, so no
// pipeline does.
func (s *VideoService) ownFramesFor(path string, req *agentpb.StreamVideoRequest) (frames ownFrames, declined string, native bool) {
	var reasons []string
	for _, source := range nativeCaptureSources {
		if why, refused := s.sourceRefusals.recent(sourceRefusalKey(path, source.format)); refused {
			reasons = append(reasons, source.format+": "+why)
			continue
		}
		frames, declined, passes := source.frames(s, path, req)
		if declined == "" {
			return frames, "", true
		}
		reasons = append(reasons, source.format+": "+declined)
		if !passes {
			return ownFrames{}, strings.Join(reasons, "; "), true
		}
	}
	return ownFrames{}, strings.Join(reasons, "; "), false
}

func (s *VideoService) nativeH264Frames(path string, req *agentpb.StreamVideoRequest) (frames ownFrames, declined string, passes bool) {
	var offered []uint32
	if s.capturePixelFormats != nil {
		offered, _ = s.capturePixelFormats(path)
	}
	if !slices.Contains(offered, v4l2PixFmtH264) {
		return ownFrames{}, "the camera offers no H.264", true
	}
	width, height := captureSizeFor(path, v4l2PixFmtH264, req.GetWidth(), req.GetHeight())
	if width == 0 || height == 0 {
		return ownFrames{}, "the camera lists no H.264 frame size a capture without one chooses, so its size is known only once it runs", false
	}
	framerate := req.GetFramerate()
	if framerate == 0 {
		framerate = highestFrameRateFor(path, v4l2PixFmtH264, width, height)
	}
	return ownFrames{codec: agentpb.VideoCodec_VIDEO_CODEC_H264, width: width, height: height, framerate: framerate}, "", false
}

var nativeH264Source = nativeCaptureSource{
	format: "H.264",
	frames: (*VideoService).nativeH264Frames,
	accept: func(s *VideoService, cam localCamera) (func(context.Context) error, string) {
		var pictures *derivedLayer
		if frames, declined, _ := s.nativeH264Frames(cam.path, cam.req); declined == "" && cam.hub != nil {
			pictures, _ = s.pictureLayerFor(cam.hub, cam.path, frames)
		}
		send := cam.sendDeciding(func() {
			if pictures != nil {
				pictures.offerTo(cam.sink)
				return
			}
			cam.sink.rawNotOffered(fixedReason(h264RawWhy))
		})
		capture := func(ctx context.Context) error { return s.streamV4L2Native(ctx, send, cam.path, cam.req) }
		if pictures == nil {
			return capture, ""
		}
		return func(ctx context.Context) error { return s.captureDeriving(ctx, cam.hub, pictures, capture) }, ""
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
	if _, err := s.derivedLayerFor(nil, videoCodecMJPEG, path, width, height, 0); err != nil {
		return 0, 0, 0, fmt.Sprintf("pictures and H.264 cannot be derived from its frames on this device: %v", err)
	}
	return width, height, nativeJPEGFrameRate(path, req, width, height), ""
}

var nativeJPEGSource = nativeCaptureSource{
	format: "JPEG",
	frames: func(s *VideoService, path string, req *agentpb.StreamVideoRequest) (ownFrames, string, bool) {
		width, height, framerate, declined := s.wholeJPEGCapture(path, req)
		return ownFrames{codec: videoCodecMJPEG, width: width, height: height, framerate: framerate}, declined, true
	},
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
		s.derivedFailures.remember(cam.path, gaveUp.msg, false)
		s.logger.Error("derived frame pipeline is unusable for this camera; GStreamer captures it instead",
			zap.String("device", cam.path), zap.Duration("retried_after", captureFailureRetryAfter), zap.String("reason", gaveUp.msg))
		// A capture that already published whole access units ends here instead:
		// consumers read a stream's alignment off its first frame, and GStreamer's
		// chunks may not follow whole units in one stream. The next hub starts on
		// GStreamer.
		return gaveUp.msg, !gaveUp.afterPublishing
	},
}

const captureFailureRetryAfter = 10 * time.Minute

// captureFailures remembers what failed for a camera, so the capture and the
// share policy ask the same thing of it next time: for a bounded time, or
// until the agent restarts when the cause is the camera's own stream.
type captureFailures struct {
	mu     sync.Mutex
	failed map[string]captureFailure
	now    func() time.Time
}

type captureFailure struct {
	why     string
	until   time.Time
	lasting bool
}

func (f *captureFailures) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

func (f *captureFailures) remember(key, why string, lasting bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed == nil {
		f.failed = make(map[string]captureFailure)
	}
	if f.failed[key].lasting {
		return
	}
	f.failed[key] = captureFailure{why: why, until: f.clock().Add(captureFailureRetryAfter), lasting: lasting}
}

func (f *captureFailures) recent(key string) (why string, failed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	failure, known := f.failed[key]
	if !known {
		return "", false
	}
	if !failure.lasting && !f.clock().Before(failure.until) {
		delete(f.failed, key)
		return "", false
	}
	return failure.why, true
}

func sourceRefusalKey(path, format string) string { return path + "\x00" + format }

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

// pictureLayerFor builds the layer that decodes pictures from a native
// capture's frames, for hub when there is one, or says why this camera has no
// pictures.
func (s *VideoService) pictureLayerFor(hub derivingHub, path string, frames ownFrames) (layer *derivedLayer, whyNot string) {
	if why, failed := s.derivedFailures.recent(path); failed {
		return nil, "decoding this camera's frames failed: " + why
	}
	layer, err := s.derivedLayerFor(hub, frames.codec, path, frames.width, frames.height, frames.framerate)
	if err != nil {
		return nil, fmt.Sprintf("pictures cannot be decoded from its frames on this device: %v", err)
	}
	if layer.pictures == nil {
		return nil, fmt.Sprintf("a %dx%d picture exceeds the %d-byte raw frame limit", frames.width, frames.height, maxRawFrameBytes)
	}
	return layer, ""
}

// A layer that gives up ends the capture when the hub's viewers get their video
// from it: they would otherwise wait on frames that will not come. A layer that
// only decodes pictures turns its pixel readers away and leaves the capture to
// the viewers.
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
		switch {
		case layerCtx.Err() != nil:
		case layer.transform.encodes:
			layerGaveUp.Store(true)
			stopCapture()
		default:
			var gaveUp derivedLayerGaveUp
			errors.As(err, &gaveUp)
			s.derivedFailures.remember(layer.device, err.Error(), gaveUp.lasting)
			hub.rawNotOffered(fixedReason("pictures cannot be decoded from this camera's frames: " + err.Error()))
			s.logger.Error("pictures cannot be decoded from this camera's frames; its own frames are still handed on",
				zap.String("device", layer.device), zap.Duration("retried_after", captureFailureRetryAfter), zap.Error(err))
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
