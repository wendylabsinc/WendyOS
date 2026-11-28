package services

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
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
		if why, refused := s.sourceRefusals.recent(s.sourceRefusalKey(cam.path, source.format)); refused {
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
		s.sourceRefusals.remember(s.sourceRefusalKey(cam.path, source.format), why, false)
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
		if why, refused := s.sourceRefusals.recent(s.sourceRefusalKey(path, source.format)); refused {
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
	frames, _, declined, passes = s.sizedNativeH264Frames(path, req)
	return frames, declined, passes
}

func (s *VideoService) sizedNativeH264Frames(path string, req *agentpb.StreamVideoRequest) (frames ownFrames, aboveDefaultLimit bool, declined string, passes bool) {
	var offered []uint32
	if s.capturePixelFormats != nil {
		offered, _ = s.capturePixelFormats(path)
	}
	if !slices.Contains(offered, v4l2PixFmtH264) {
		return ownFrames{}, false, "the camera offers no H.264", true
	}
	width, height, aboveDefaultLimit := nativeH264CaptureSize(path, req)
	if width == 0 || height == 0 {
		return ownFrames{}, false, "the camera lists no H.264 frame size a capture without one chooses, so its size is known only once it runs", false
	}
	framerate := req.GetFramerate()
	if framerate == 0 {
		framerate = highestFrameRateFor(path, v4l2PixFmtH264, width, height)
	}
	return ownFrames{codec: agentpb.VideoCodec_VIDEO_CODEC_H264, width: width, height: height, framerate: framerate}, aboveDefaultLimit, "", false
}

// A node declares its size before the capture runs, so one is chosen even
// when none the camera lists fits the default limit.
func nativeH264CaptureSize(path string, req *agentpb.StreamVideoRequest) (width, height uint32, aboveDefaultLimit bool) {
	width, height = captureSizeFor(path, v4l2PixFmtH264, req.GetWidth(), req.GetHeight())
	if width != 0 && height != 0 {
		return width, height, false
	}
	width, height = smallestFrameSize(enumerateRawFrameSizes(path, v4l2PixFmtH264))
	return width, height, width != 0 && height != 0
}

func smallestFrameSize(sizes [][2]uint32) (width, height uint32) {
	var anyWidth, anyHeight uint32
	for _, size := range sizes {
		area := uint64(size[0]) * uint64(size[1])
		if area == 0 {
			continue
		}
		if anyWidth == 0 || area < uint64(anyWidth)*uint64(anyHeight) {
			anyWidth, anyHeight = size[0], size[1]
		}
		if hasStandardAspect(size[0], size[1]) && (width == 0 || area < uint64(width)*uint64(height)) {
			width, height = size[0], size[1]
		}
	}
	if width == 0 {
		return anyWidth, anyHeight
	}
	return width, height
}

var nativeH264Source = nativeCaptureSource{
	format: "H.264",
	frames: (*VideoService).nativeH264Frames,
	accept: func(s *VideoService, cam localCamera) (func(context.Context) error, string) {
		var pictures *derivedLayer
		width, height := cam.req.GetWidth(), cam.req.GetHeight()
		frames, aboveDefaultLimit, declined, _ := s.sizedNativeH264Frames(cam.path, cam.req)
		if declined == "" {
			width, height = frames.width, frames.height
			if cam.hub != nil {
				pictures, _ = s.pictureLayerFor(cam.hub, cam.path, frames)
			}
		}
		send := cam.sendDeciding(func() {
			if pictures != nil {
				pictures.offerTo(cam.sink)
				return
			}
			cam.sink.rawNotOffered(fixedReason(h264RawWhy))
		})
		capture := func(ctx context.Context) error {
			if aboveDefaultLimit {
				s.logger.Info("selected an H.264 capture size above the default limit: the camera lists none within it",
					zap.String("device", cam.path), zap.Uint32("width", width), zap.Uint32("height", height),
					zap.Uint32("limit_width", defaultMaxDefaultWidth), zap.Uint32("limit_height", defaultMaxDefaultHeight))
			}
			return s.streamV4L2Native(ctx, send, cam.path, width, height, cam.req.GetFramerate())
		}
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
	if why, failed := s.derivedFailures.recent(s.cameraIdentityKey(path)); failed {
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
		s.derivedFailures.remember(s.cameraIdentityKey(cam.path), gaveUp.msg, false)
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

// captureFailures remembers what failed for a camera (cameraIdentityKey), so the
// capture and the share policy ask the same thing of it next time: for a
// bounded time, or until the agent restarts when the cause is the camera's own
// stream.
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

// cameraSelf is what a camera node says of itself wherever it is plugged in.
type cameraSelf struct {
	usb         camera.USB
	index, name string
}

// cameraNode is one camera node as sysfs has it now. unread: what its USB
// device says could not be read, so usb is not known.
type cameraNode struct {
	id     camera.Identity
	usb    camera.USB
	unread bool
}

func (n cameraNode) self() cameraSelf {
	return cameraSelf{usb: n.usb, index: n.id.Index, name: n.id.Name}
}

// knownCameras holds the cameras sysfs has shown since the agent started, each
// with the port it was last at, and the camera last named at each path. udev's
// names are not used: it names a camera some time after the kernel has made
// its node, and a capture that starts in between must find the same camera.
type knownCameras struct {
	mu     sync.Mutex
	seen   []*knownCamera
	lastAt map[string]*knownCamera
}

type knownCamera struct {
	key  string
	self cameraSelf
	port string
	// stray: nodes of its kind appeared while it was away and it could not be
	// told which, if any, it was. It is known again at port only, and does
	// not count among the cameras one that appears later may be.
	stray bool
}

// cameraIdentityKey names the camera at path for what belongs to the camera
// and not to its path or port: its failure memory and its sample counter. It
// goes by what sysfs shows each time it is asked, which cannot tell a camera
// without a serial of its own from an identical one found on its port, or on
// another port with the first gone, since it last looked: such a camera goes
// on with the first one's counter and failure memory. A node sysfs has no
// camera for is named by its path.
func (s *VideoService) cameraIdentityKey(path string) string {
	base := filepath.Base(path)
	id, err := s.identityOf(base)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && id == (camera.Identity{})) {
		return path
	}
	if err != nil {
		return s.cameras.lastNamedAt(path)
	}
	present := s.presentIdentities()
	nodes := make([]cameraNode, 0, len(present)+1)
	for other, otherID := range present {
		if other != base {
			nodes = append(nodes, s.cameraNode(other, otherID))
		}
	}
	own := s.cameraNode(base, id)
	return s.cameras.name(path, own, append(nodes, own))
}

func (s *VideoService) cameraNode(base string, id camera.Identity) cameraNode {
	if s.cameraUSB == nil {
		return cameraNode{id: id}
	}
	usb, err := s.cameraUSB(base)
	return cameraNode{id: id, usb: usb, unread: err != nil}
}

// name is the key of the camera at path, which is own, one of the nodes
// present. A node that cannot be read is no new camera: it is the camera last
// named at path if that one was at its port, else the only known camera there.
// present was listed before the lock was taken, so two listings can be seen
// out of order; the next one puts the ports right, and no key changes.
func (k *knownCameras) name(path string, own cameraNode, present []cameraNode) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.see(present)
	var named *knownCamera
	if !own.unread {
		named = k.at(own.id, func(known *knownCamera) bool { return known.self == own.self() })
	} else if last := k.lastAt[path]; last != nil && last.sits(own.id) {
		named = last
	} else {
		named = k.at(own.id, func(*knownCamera) bool { return true })
	}
	if named == nil {
		return path
	}
	if k.lastAt == nil {
		k.lastAt = make(map[string]*knownCamera)
	}
	k.lastAt[path] = named
	return named.key
}

// lastNamedAt is the key for a node whose identity cannot be read now.
func (k *knownCameras) lastNamedAt(path string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if last := k.lastAt[path]; last != nil {
		return last.key
	}
	return path
}

func (c *knownCamera) sits(id camera.Identity) bool {
	return c.port == id.Device && c.self.index == id.Index && c.self.name == id.Name
}

// at is the known camera at id's port with id's index and name that is one of
// those asked for, or nil when there is none or more than one.
func (k *knownCameras) at(id camera.Identity, asked func(*knownCamera) bool) *knownCamera {
	var found *knownCamera
	for _, known := range k.seen {
		if !known.sits(id) || !asked(known) {
			continue
		}
		if found != nil {
			return nil
		}
		found = known
	}
	return found
}

// see brings the known cameras up to the nodes present. A camera whose kind
// has a node at its port is there. A node at a port no known camera of its
// kind is at is the one camera of that kind that is away, when there is one
// such node and one such camera; otherwise each such node is a new camera and
// the ones away are strays. A camera at the port of a node that cannot be
// read stays where it is.
func (k *knownCameras) see(present []cameraNode) {
	unclaimed := map[cameraSelf]map[string]bool{}
	unread := map[camera.Identity]bool{}
	for _, node := range present {
		if node.unread {
			unread[node.id] = true
			continue
		}
		if unclaimed[node.self()] == nil {
			unclaimed[node.self()] = map[string]bool{}
		}
		unclaimed[node.self()][node.id.Device] = true
	}
	away := map[cameraSelf][]*knownCamera{}
	for _, known := range k.seen {
		switch {
		case unclaimed[known.self][known.port]:
			delete(unclaimed[known.self], known.port)
			known.stray = false
		case unread[camera.Identity{Device: known.port, Index: known.self.index, Name: known.self.name}]:
		case !known.stray:
			away[known.self] = append(away[known.self], known)
		}
	}
	for self, ports := range unclaimed {
		if len(ports) == 0 {
			continue
		}
		if len(ports) == 1 && len(away[self]) == 1 {
			for port := range ports {
				away[self][0].port = port
			}
			continue
		}
		for _, known := range away[self] {
			known.stray = true
		}
		for port := range ports {
			k.seen = append(k.seen, &knownCamera{key: fmt.Sprintf("camera\x00%d", len(k.seen)), self: self, port: port})
		}
	}
}

func (s *VideoService) sourceRefusalKey(path, format string) string {
	return s.cameraIdentityKey(path) + "\x00" + format
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

// pictureLayerFor builds the layer that decodes pictures from a native
// capture's frames, for hub when there is one, or says why this camera has no
// pictures.
func (s *VideoService) pictureLayerFor(hub derivingHub, path string, frames ownFrames) (layer *derivedLayer, whyNot string) {
	if why, failed := s.derivedFailures.recent(s.cameraIdentityKey(path)); failed {
		return nil, "decoding this camera's frames failed: " + why
	}
	layer, err := s.derivedLayerFor(hub, frames.codec, path, frames.width, frames.height, frames.framerate)
	if err != nil {
		return nil, fmt.Sprintf("pictures cannot be decoded from its frames on this device: %v", err)
	}
	if layer.pictures == nil {
		return nil, pictureTooLargeWhy(frames.width, frames.height)
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
			s.derivedFailures.remember(s.cameraIdentityKey(layer.device), err.Error(), gaveUp.lasting)
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
