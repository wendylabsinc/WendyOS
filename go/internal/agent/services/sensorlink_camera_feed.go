package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// A camera paired over SensorLink (mcusource) reaches the agent as whole
// frames, which are written into a v4l2loopback node in the MCU band so apps
// can open it like any camera. The hub must not read those frames back from
// that node: v4l2loopback serves one streaming reader at a time. MEASURED on
// v4l2loopback 0.15.4 (demo-iq9, 2026-10-19): while an app streamed from
// /dev/video96, a second opener got EBUSY from VIDIOC_S_FMT, VIDIOC_REQBUFS and
// read() alike, and only G_FMT succeeded. The module's max_openers counts
// opens, not readers. Reading the node made the hub and any app reading the
// camera directly mutually exclusive, so `wendy data` could not record a
// SensorLink camera while a viewer showed it.
//
// SensorFrameFeed hands the frames to the hub in-process instead: the writer
// that fills the node also offers each frame here, and the hub never opens the
// node.
type SensorFrameFeed struct {
	mu   sync.Mutex
	subs map[string]map[chan ros2camera.Frame]struct{}
}

// sensorFrameBuffer is how many frames a subscriber may fall behind before the
// oldest is dropped. A slow encoder must never stall the SensorLink reader,
// which also feeds the node apps read.
const sensorFrameBuffer = 4

// sensorFrameStall bounds the wait for a SensorLink camera's next frame,
// including its first. The feed outlives the node writer, so a board that
// drops off Wi-Fi and reconnects resumes the same stream; only a camera that
// stays silent ends it. A var so tests can shorten it.
var sensorFrameStall = 30 * time.Second

// NewSensorFrameFeed returns an empty feed. Wire it into both sides at
// startup: VideoService.SetSensorFrameFeed, and WrapWriters around the frame
// writer factory mcusource is given.
func NewSensorFrameFeed() *SensorFrameFeed {
	return &SensorFrameFeed{subs: make(map[string]map[chan ros2camera.Frame]struct{})}
}

// WrapWriters returns a writer factory that offers every frame written to a
// node to the feed's subscribers for that node's path, before writing it to the
// node with newWriter's writer.
func (f *SensorFrameFeed) WrapWriters(newWriter func(path string) ros2camera.CameraWriter) func(path string) ros2camera.CameraWriter {
	return func(path string) ros2camera.CameraWriter {
		return &sensorFeedWriter{feed: f, path: path, next: newWriter(path)}
	}
}

type sensorFeedWriter struct {
	feed *SensorFrameFeed
	path string
	next ros2camera.CameraWriter
}

// WriteFrame publishes first, so the hub keeps its frames even when the node
// cannot be written (for example a kernel without v4l2loopback).
func (w *sensorFeedWriter) WriteFrame(frame ros2camera.Frame) error {
	w.feed.publish(w.path, frame)
	return w.next.WriteFrame(frame)
}

// Close closes the node writer only. Subscribers stay subscribed, so a link
// that reconnects with a new writer for the same node continues their stream.
func (w *sensorFeedWriter) Close() error { return w.next.Close() }

func (f *SensorFrameFeed) subscribe(path string) (<-chan ros2camera.Frame, func()) {
	ch := make(chan ros2camera.Frame, sensorFrameBuffer)
	f.mu.Lock()
	if f.subs[path] == nil {
		f.subs[path] = make(map[chan ros2camera.Frame]struct{})
	}
	f.subs[path][ch] = struct{}{}
	f.mu.Unlock()
	return ch, func() {
		f.mu.Lock()
		delete(f.subs[path], ch)
		if len(f.subs[path]) == 0 {
			delete(f.subs, path)
		}
		f.mu.Unlock()
	}
}

func (f *SensorFrameFeed) publish(path string, frame ros2camera.Frame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	subs := f.subs[path]
	if len(subs) == 0 {
		return
	}
	// Subscribers hold a frame while the link reads the next one, so they get
	// their own copy rather than the transport's buffer. They may also read it
	// later than now, so it says when it arrived.
	frame.Data = bytes.Clone(frame.Data)
	if frame.Received.IsZero() {
		frame.Received = time.Now()
	}
	for ch := range subs {
		select {
		case ch <- frame:
			continue
		default:
		}
		// Full: drop the oldest, so a subscriber that catches up encodes what
		// the camera sees now rather than what it saw a moment ago.
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- frame:
		default:
		}
	}
}

// SetSensorFrameFeed makes the hub capture SensorLink cameras from feed rather
// than from their nodes. Call once at startup, before serving.
func (s *VideoService) SetSensorFrameFeed(feed *SensorFrameFeed) { s.sensorFrames = feed }

// isSensorLinkCameraPath reports whether path is a node in the MCU band, where
// mcusource mounts SensorLink cameras.
func isSensorLinkCameraPath(path string) bool {
	num, ok := strings.CutPrefix(path, "/dev/video")
	if !ok {
		return false
	}
	id, err := strconv.ParseUint(num, 10, 32)
	return err == nil && id >= ipcam.MCUBandStart && id <= ipcam.MCUBandEnd
}

// errSensorFeedNotJPEG sends a SensorLink camera that streams H.264 back to
// the node path: the feed encodes JPEG only, and no board sends H.264 yet to
// verify a pass-through against.
var errSensorFeedNotJPEG = errors.New("SensorLink camera does not send JPEG")

const sensorLinkRawWhy = "SensorLink camera sends JPEG; raw frames are not offered"

// streamSensorLinkCamera encodes a SensorLink camera's JPEG frames from the
// feed onto the hub, without opening its node.
func (s *VideoService) streamSensorLinkCamera(ctx context.Context, broadcast func([]byte, frameTimestamp, agentpb.VideoCodec) bool, path string, req *agentpb.StreamVideoRequest, sink rawSink) error {
	frames, unsubscribe := s.sensorFrames.subscribe(path)
	defer unsubscribe()
	first, err := nextSensorFrame(ctx, frames)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return status.Errorf(codes.Unavailable, "SensorLink camera %s is not streaming: %v", path, err)
	}
	if first.Codec != ros2camera.CodecMJPEG {
		return errSensorFeedNotJPEG
	}
	if req.GetCodec() == agentpb.VideoCodec_VIDEO_CODEC_RAW {
		return errRawUnavailable(sensorLinkRawWhy)
	}
	// The frame's own header is authoritative for the pipeline's caps; the
	// size the board declared at pairing is only what it promised.
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(first.Data))
	if err != nil {
		return status.Errorf(codes.DataLoss, "SensorLink camera %s sent an unreadable JPEG: %v", path, err)
	}
	gst, enc, err := resolveJPEGFeedEncoder()
	if err != nil {
		return err
	}
	plan, err := planJPEGFeedPipeline(gst, path, req, enc, cfg.Width, cfg.Height, "SensorLink camera capture", sensorLinkRawWhy)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	pump := func(ctx context.Context, w io.Writer) error {
		return pumpSensorFrames(ctx, w, frames, first)
	}
	return s.runJPEGFeed(ctx, broadcast, path, sink, enc, plan, pump, fmt.Sprintf("SensorLink camera %s stopped", path))
}

// pumpSensorFrames writes first and every following frame to out until ctx
// ends, the camera goes silent for sensorFrameStall, or its format changes:
// the pipeline's caps are fixed for the life of the stream, so a new format
// needs a new producer.
func pumpSensorFrames(ctx context.Context, out io.Writer, frames <-chan ros2camera.Frame, first ros2camera.Frame) error {
	frame := first
	for {
		if _, err := out.Write(frame.Data); err != nil {
			return err
		}
		next, err := nextSensorFrame(ctx, frames)
		if err != nil {
			return err
		}
		if next.Codec != first.Codec || next.Width != first.Width || next.Height != first.Height {
			return fmt.Errorf("camera changed from %dx%d to %dx%d", first.Width, first.Height, next.Width, next.Height)
		}
		frame = next
	}
}

func nextSensorFrame(ctx context.Context, frames <-chan ros2camera.Frame) (ros2camera.Frame, error) {
	timer := time.NewTimer(sensorFrameStall)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ros2camera.Frame{}, ctx.Err()
	case frame := <-frames:
		return frame, nil
	case <-timer.C:
		return ros2camera.Frame{}, fmt.Errorf("no frame for %s", sensorFrameStall)
	}
}
