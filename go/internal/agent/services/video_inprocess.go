package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/rawvideo"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// In-process cameras.
//
// Every other camera the hub encodes is something GStreamer can open: a
// /dev/videoN node, an RTSP URL, a PipeWire node. A camera with a userspace
// driver (the TOPDON TC001 Max is the first) has none of those. Its frames
// exist only inside the agent, as rawvideo.Frame values the driver publishes to
// a rawvideo.Feed. RegisterInProcessCamera tells the hub about such a camera,
// and from then on StreamVideo's encoded path serves its device ID from the
// feed instead of opening /dev/video<id>.
//
// The encoding is the hub's ordinary GStreamer encoding, not a second stack:
// the same encoder choice (findGStreamerEncoderFromSet), the same encoder
// settings and keyframe interval (encoderSegment, keyframeIntervalFrames), the
// same leaky raw queue, and the same subprocess, teardown, first-frame timeout
// and error reporting (runJPEGFeed and runCameraPipeline, which the SensorLink
// and shared-JPEG feeds already use to encode frames written to the pipeline's
// stdin). The only new stage is how frames enter the pipeline: whole raw frames
// written to stdin and framed by rawvideoparse, where those feeds write JPEG
// framed by jpegparse.

// inProcessHubKeyPrefix marks a hub key as an in-process camera's, the way
// ipHubKeyPrefix marks a network camera's. The hub map is keyed by opaque
// strings, so the kinds share one map without colliding.
const inProcessHubKeyPrefix = "inproc:"

// inProcessRawWhy is why a VIDEO_CODEC_RAW request for an in-process camera is
// refused. The frames are in the agent already; offering them as raw is the
// driver's own business (the raw tap), not this encoded path's.
const inProcessRawWhy = "this camera's frames come from an in-process driver; raw frames are not offered on its live view"

// inProcessFrameStall bounds the wait for an in-process camera's next frame,
// including its first. A thermal camera pauses for its shutter calibration
// (well under a second); only a camera that stays silent this long ends the
// stream. Kept under the dashboard player's 25 s budget so this error, which
// names the cause, reaches a viewer before its own timeout does. A var so
// tests can shorten it.
var inProcessFrameStall = 10 * time.Second

// defaultInProcessFramerate is the rate the pipeline is told to expect when the
// camera's frames carry no usable timestamps. It only paces the encoder's rate
// control and sets the keyframe interval: frames still enter the pipeline as
// fast as the camera publishes them.
const defaultInProcessFramerate = 25

// maxInProcessFramerate caps the measured rate. Nothing in a live view needs
// more, and the keyframe interval is in frames.
const maxInProcessFramerate = 60

// errInProcessCameraGone ends a stream whose camera was unregistered while it
// was being watched (unplugged, or its driver stopped).
var errInProcessCameraGone = errors.New("camera was disconnected")

// inProcessCamera is one registered camera.
type inProcessCamera struct {
	feed *rawvideo.Feed
	// gone is closed when the camera is unregistered, ending its stream.
	gone chan struct{}
}

// inProcessCameras is the registry, embedded in VideoService. Its own lock, so
// registering a camera never contends with the hub map.
type inProcessCameras struct {
	inProcessMu   sync.Mutex
	inProcessByID map[uint32]*inProcessCamera
}

// RegisterInProcessCamera makes StreamVideo serve device ID id from feed: the
// encoded (default) path pulls frames from the feed and encodes them like any
// local camera, starting when the first viewer joins and stopping when the last
// leaves. Call unregister when the camera goes away; a viewer still watching
// then gets Unavailable ("camera was disconnected"). unregister may be called
// more than once.
//
// The registration takes precedence over every other source StreamVideo knows
// for id, so id must be one the caller owns: a device ID it reports for this
// camera and nothing else (for example the number of its own loopback node, if
// it writes one for apps, so the hub never reads that node back). It must be
// at most 255, the range StreamVideo accepts, and not already registered.
//
// Registration does not list the camera; ListVideoDevices is the driver's to
// extend. Nor does it offer raw frames: a VIDEO_CODEC_RAW request for id is
// refused with RAW_UNAVAILABLE (see inProcessRawWhy).
func (s *VideoService) RegisterInProcessCamera(id uint32, feed *rawvideo.Feed) (unregister func(), err error) {
	if feed == nil {
		return nil, errors.New("in-process camera needs a feed")
	}
	if id > maxVideoDeviceID {
		return nil, fmt.Errorf("camera id %d is out of range (0..%d)", id, maxVideoDeviceID)
	}
	cam := &inProcessCamera{feed: feed, gone: make(chan struct{})}
	s.inProcessMu.Lock()
	defer s.inProcessMu.Unlock()
	if s.inProcessByID == nil {
		s.inProcessByID = make(map[uint32]*inProcessCamera)
	}
	if _, taken := s.inProcessByID[id]; taken {
		return nil, fmt.Errorf("camera id %d is already registered as an in-process camera", id)
	}
	s.inProcessByID[id] = cam
	var once sync.Once
	return func() {
		once.Do(func() {
			s.inProcessMu.Lock()
			if s.inProcessByID[id] == cam {
				delete(s.inProcessByID, id)
			}
			s.inProcessMu.Unlock()
			close(cam.gone)
		})
	}, nil
}

// inProcessCameraByID returns the camera registered under id.
func (s *VideoService) inProcessCameraByID(id uint32) (*inProcessCamera, bool) {
	s.inProcessMu.Lock()
	defer s.inProcessMu.Unlock()
	cam, ok := s.inProcessByID[id]
	return cam, ok
}

// inProcessHubKey is the hub key for an in-process camera's producer.
func inProcessHubKey(id uint32) string {
	return inProcessHubKeyPrefix + strconv.FormatUint(uint64(id), 10)
}

// inProcessCameraForKey resolves a hub key back to its camera.
func (s *VideoService) inProcessCameraForKey(key string) (*inProcessCamera, bool) {
	id, err := strconv.ParseUint(strings.TrimPrefix(key, inProcessHubKeyPrefix), 10, 32)
	if err != nil {
		return nil, false
	}
	return s.inProcessCameraByID(uint32(id))
}

// streamInProcessCamera is StreamVideo for a registered in-process camera.
// handled is false when devID names no such camera, and StreamVideo goes on to
// resolve it as before.
func (s *VideoService) streamInProcessCamera(stream grpc.ServerStreamingServer[agentpb.VideoFrame], devID uint32, req *agentpb.StreamVideoRequest) (handled bool, err error) {
	if _, ok := s.inProcessCameraByID(devID); !ok {
		return false, nil
	}
	// Refused before a hub exists, as for a network camera: a codec this path
	// cannot produce is refused rather than downgraded (see the proto).
	if req.GetCodec() == agentpb.VideoCodec_VIDEO_CODEC_RAW {
		return true, errRawUnavailable(inProcessRawWhy)
	}
	if err := checkInProcessRequest(req); err != nil {
		return true, err
	}
	ctx := stream.Context()
	h, id, ch, err := s.getOrCreateHub(ctx, inProcessHubKey(devID), req, hubHolderStreamClient)
	if err != nil {
		return true, err
	}
	defer h.unsubscribe(id)
	return true, s.pumpFrames(stream, h, id, ch)
}

// checkInProcessRequest refuses, before any producer starts, the stream
// parameters an in-process camera can never honour. The size is checked
// against the camera's own once its first frame says what that is
// (checkInProcessSize).
func checkInProcessRequest(req *agentpb.StreamVideoRequest) error {
	if req.GetFramerate() != 0 {
		// Same answer as the other feed-driven sources (planJPEGFeedPipeline):
		// frames arrive at the camera's pace, so a rate cannot be promised.
		return status.Error(codes.InvalidArgument,
			"in-process camera capture uses the camera's own frame timing; omit the requested frame rate")
	}
	if (req.GetWidth() == 0) != (req.GetHeight() == 0) {
		return status.Error(codes.InvalidArgument, "width and height must be set together")
	}
	return nil
}

// checkInProcessSize accepts the camera's native size (or no size), and
// otherwise the sizes validateStreamParams accepts for a source it cannot
// enumerate, to which the pipeline then scales.
func checkInProcessSize(req *agentpb.StreamVideoRequest, width, height int) error {
	w, h := req.GetWidth(), req.GetHeight()
	if (w == 0 && h == 0) || (int(w) == width && int(h) == height) {
		return nil
	}
	return validateStreamParams("", req)
}

// runInProcessProducer encodes an in-process camera's feed onto its hub. It is
// runProducer's capture step for an inProcessHubKeyPrefix key.
//
// A format change, a malformed frame, a silent camera, an unregistered camera
// and an encoder failure all end the producer with an error that names the
// cause, and the next StreamVideo call starts a fresh pipeline (with the new
// format, after a change). The stream is ended rather than restarted under the
// viewer, for the reason the hub never splices producers (takeOverDefaultedHub,
// pumpSensorFrames): a new pipeline means a new sequence parameter set, and a
// decoder handed two in one timeline, or a WebM header mid-stream, produces
// garbage.
func (s *VideoService) runInProcessProducer(ctx context.Context, broadcast func([]byte, frameTimestamp, agentpb.VideoCodec) bool, key string, req *agentpb.StreamVideoRequest, sink rawSink) error {
	// Settled before anything else, so a raw subscriber is never left waiting.
	sink.rawNotOffered(fixedReason(inProcessRawWhy))
	label := "in-process camera " + strings.TrimPrefix(key, inProcessHubKeyPrefix)
	cam, ok := s.inProcessCameraForKey(key)
	if !ok {
		return status.Errorf(codes.Unavailable, "%s: %v", label, errInProcessCameraGone)
	}
	frames, unsubscribe := cam.feed.Subscribe()
	defer unsubscribe()

	notStreaming := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return status.Errorf(codes.Unavailable, "%s is not streaming: %v", label, err)
	}
	first, err := nextInProcessFrame(ctx, frames, cam.gone)
	if err != nil {
		return notStreaming(err)
	}
	// The first frame decides the pipeline's caps; they are fixed for its life.
	in, err := inProcessInputFor(first)
	if err != nil {
		return status.Errorf(codes.DataLoss, "%s sent an unusable frame: %v", label, err)
	}
	if err := checkInProcessSize(req, first.Width, first.Height); err != nil {
		return err
	}
	pending := []rawvideo.Frame{first}
	if cam.feed.FrameInterval() <= 0 {
		// A driver that publishes only while watched has not been measured
		// yet. One more frame measures it; it is encoded, not discarded.
		second, err := nextInProcessFrame(ctx, frames, cam.gone)
		if err != nil {
			return notStreaming(err)
		}
		pending = append(pending, second)
	}
	fps := inProcessFramerate(cam.feed.FrameInterval())

	gst, enc, err := resolveInProcessEncoder()
	if err != nil {
		return err
	}
	plan, err := planInProcessPipeline(gst, req, enc, in, fps)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	s.logger.Info("encoding in-process camera", zap.String("camera", label),
		zap.String("format", first.Describe()), zap.Uint32("framerate", fps),
		zap.String("encoder", enc.element))
	pump := func(ctx context.Context, w io.Writer) error {
		return pumpInProcessFrames(ctx, w, frames, cam.gone, in, pending)
	}
	// runJPEGFeed is format-agnostic despite its name: it runs plan with
	// whatever pump writes as the pipeline's stdin, and reports a pump that
	// fails while the hub still wants frames as Unavailable.
	return s.runJPEGFeed(ctx, broadcast, key, noRawSink{}, enc, plan, pump, label+" stopped")
}

// resolveInProcessEncoder finds what an in-process camera pipeline needs: the
// GStreamer binary, the encoder resolveJPEGFeedEncoder would choose, and
// rawvideoparse (the rawparse plugin, gst-plugins-base since 1.20) to cut the
// byte stream on stdin back into frames.
func resolveInProcessEncoder() (string, gstEncoderResult, error) {
	gst, err := resolveGSTBinary("gst-launch-1.0")
	if err != nil {
		return "", gstEncoderResult{}, status.Error(codes.FailedPrecondition, err.Error())
	}
	inspect, err := resolveGSTBinary("gst-inspect-1.0")
	if err != nil {
		return "", gstEncoderResult{}, status.Error(codes.FailedPrecondition, err.Error())
	}
	// A failed listing leaves available nil, which findGStreamerEncoderFromSet
	// answers with x264enc, as findGStreamerEncoder does; gst-launch then names
	// whatever is really missing.
	available, listErr := listGSTElements(inspect)
	if listErr == nil && !available["rawvideoparse"] {
		return "", gstEncoderResult{}, status.Error(codes.FailedPrecondition,
			"GStreamer element rawvideoparse is missing; install the GStreamer rawparse plugin (gst-plugins-base)")
	}
	enc, err := findGStreamerEncoderFromSet(available)
	if err != nil {
		return "", gstEncoderResult{}, status.Error(codes.FailedPrecondition, err.Error())
	}
	return gst, enc, nil
}

// inProcessFramerate turns a measured frame interval into the integer rate the
// pipeline's caps declare.
func inProcessFramerate(interval time.Duration) uint32 {
	if interval <= 0 {
		return defaultInProcessFramerate
	}
	fps := math.Round(float64(time.Second) / float64(interval))
	return uint32(min(max(fps, 1), maxInProcessFramerate))
}

// inProcessInput is how an in-process camera's frames are written to the
// pipeline: the format the pipeline is told it reads, and the row layout.
type inProcessInput struct {
	source        string // rawvideo format as the camera publishes it
	gstFormat     string // raw format written to the pipeline
	width, height int
	// rowBytes is one row as written, tightly packed; stride is the same row
	// padded to GStreamer's default stride. rawvideoparse reads video/x-raw
	// caps with default strides, which round every row up to 4 bytes
	// (MEASURED with GStreamer 1.28: 250-pixel GRAY8 rows written tightly
	// framed 10 frames as 9). A frame is published tightly packed, so rows are
	// padded on the way in whenever the two differ.
	rowBytes, stride int
}

// inProcessInputFor derives the pipeline input from a camera's first frame.
//
// GRAY16_LE is stretched to GRAY8 here, in the agent, rather than handed to
// GStreamer as GRAY16: videoconvert would keep the high byte, which shows a
// thermal or depth sensor's narrow band of values as one flat grey. See
// rawvideo.StretchGray16 for the mapping.
func inProcessInputFor(f rawvideo.Frame) (inProcessInput, error) {
	if err := f.Validate(); err != nil {
		return inProcessInput{}, err
	}
	in := inProcessInput{source: f.Format, gstFormat: f.Format, width: f.Width, height: f.Height}
	bpp, _ := rawvideo.BytesPerPixel(f.Format)
	if f.Format == rawvideo.FormatGRAY16LE {
		in.gstFormat, bpp = rawvideo.FormatGRAY8, 1
	}
	in.rowBytes = f.Width * bpp
	in.stride = (in.rowBytes + 3) &^ 3
	return in, nil
}

// sameShape reports whether f has the format and size the pipeline was built for.
func (in inProcessInput) sameShape(f rawvideo.Frame) bool {
	return f.Format == in.source && f.Width == in.width && f.Height == in.height
}

func (in inProcessInput) describe() string {
	return fmt.Sprintf("%dx%d %s", in.width, in.height, in.source)
}

// inProcessPacker converts frames to the pipeline's input layout, reusing its
// buffers from frame to frame.
type inProcessPacker struct {
	in            inProcessInput
	gray8, padded []byte
}

// pack returns f's bytes as the pipeline reads them. The result may alias f.Data
// or the packer's buffers, so it is valid only until the next call; io.Pipe's
// Write has handed every byte to the reader before it returns.
func (p *inProcessPacker) pack(f rawvideo.Frame) []byte {
	pix := f.Data
	if p.in.source == rawvideo.FormatGRAY16LE {
		p.gray8 = rawvideo.StretchGray16(p.gray8, f.Data)
		pix = p.gray8
	}
	if p.in.stride == p.in.rowBytes {
		return pix
	}
	need := p.in.stride * p.in.height
	if cap(p.padded) < need {
		p.padded = make([]byte, need)
	}
	out := p.padded[:need]
	for y := 0; y < p.in.height; y++ {
		row := out[y*p.in.stride : (y+1)*p.in.stride]
		n := copy(row, pix[y*p.in.rowBytes:(y+1)*p.in.rowBytes])
		clear(row[n:])
	}
	return out
}

// planInProcessPipeline builds the gst-launch arguments that encode raw frames
// written to stdin. Everything after the frames are parsed is the hub's usual
// encoding tail.
//
// No upscaling is added for small cameras: the V4L2 path already encodes the
// TC001's 256x192 picture at that size (videocrop of its stacked mode), and
// viewers scale on display. The only scaling is to a size the viewer asked for,
// or up by one pixel to even dimensions, which 4:2:0 H.264 encoders require
// (x264enc refuses to initialise at 255x191).
func planInProcessPipeline(gst string, req *agentpb.StreamVideoRequest, enc gstEncoderResult, in inProcessInput, fps uint32) (gstPipelinePlan, error) {
	if !isValidGSTElementName(enc.element) {
		return gstPipelinePlan{}, errors.New("invalid in-process camera encoder")
	}
	if _, ok := rawvideo.BytesPerPixel(in.gstFormat); !ok {
		// Only the fixed format names above ever reach the caps string.
		return gstPipelinePlan{}, fmt.Errorf("unsupported pipeline format %q", in.gstFormat)
	}
	outW, outH := in.width, in.height
	if req.GetWidth() != 0 && req.GetHeight() != 0 {
		outW, outH = int(req.GetWidth()), int(req.GetHeight())
	}
	outW, outH = outW+outW%2, outH+outH%2
	stages := []string{
		"fdsrc fd=0 do-timestamp=true",
		fmt.Sprintf("video/x-raw,format=%s,width=%d,height=%d,framerate=%d/1", in.gstFormat, in.width, in.height, fps),
		"rawvideoparse use-sink-caps=true",
		leakyRawQueue,
	}
	if outW != in.width || outH != in.height {
		stages = append(stages, "videoscale", fmt.Sprintf("video/x-raw,width=%d,height=%d", outW, outH))
	}
	stages = append(stages, encoderSegment(enc.element, enc.hasH264Parse, keyframeIntervalFrames(fps)), "fdsink fd=1")
	return gstPipelinePlan{
		// -q keeps gst-launch's status text off stdout, which carries the video.
		args:   append([]string{gst, "-q"}, strings.Fields(strings.Join(stages, " ! "))...),
		rawWhy: inProcessRawWhy,
	}, nil
}

// pumpInProcessFrames writes pending and then every following frame to out,
// until ctx ends, the camera is unregistered, goes silent for
// inProcessFrameStall, publishes a malformed frame, or changes format: the
// pipeline's caps are fixed for its life, so a new format needs a new producer.
//
// Backpressure never reaches the driver. If the encoder falls behind, this
// loop blocks on the pipe while the feed drops its oldest queued frames, and
// the pipeline's leaky queue drops raw frames the encoder has no time for.
func pumpInProcessFrames(ctx context.Context, out io.Writer, frames <-chan rawvideo.Frame, gone <-chan struct{}, in inProcessInput, pending []rawvideo.Frame) error {
	packer := inProcessPacker{in: in}
	write := func(f rawvideo.Frame) error {
		if !in.sameShape(f) {
			return fmt.Errorf("camera changed from %s to %s; reconnect to view the new format", in.describe(), f.Describe())
		}
		if err := f.Validate(); err != nil {
			return fmt.Errorf("camera sent a malformed frame: %w", err)
		}
		_, err := out.Write(packer.pack(f))
		return err
	}
	for _, f := range pending {
		if err := write(f); err != nil {
			return err
		}
	}
	for {
		f, err := nextInProcessFrame(ctx, frames, gone)
		if err != nil {
			return err
		}
		if err := write(f); err != nil {
			return err
		}
	}
}

// nextInProcessFrame waits for the camera's next frame.
func nextInProcessFrame(ctx context.Context, frames <-chan rawvideo.Frame, gone <-chan struct{}) (rawvideo.Frame, error) {
	timer := time.NewTimer(inProcessFrameStall)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return rawvideo.Frame{}, ctx.Err()
	case <-gone:
		return rawvideo.Frame{}, errInProcessCameraGone
	case f := <-frames:
		return f, nil
	case <-timer.C:
		return rawvideo.Frame{}, fmt.Errorf("no frame for %s", inProcessFrameStall)
	}
}
