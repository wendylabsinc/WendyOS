package services

import (
	"bytes"
	"context"
	"fmt"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const nativeJPEGUnusableBuffersBeforeNotSupported = 10

type nativeJPEGNotSupported struct{ msg string }

func (e nativeJPEGNotSupported) Error() string { return e.msg }

// Webcams follow the end marker with zeros, FF bytes or vendor data, so the
// frame ends at the last end marker in the buffer.
func jpegFrameInBuffer(buffer []byte) (frame []byte, ok bool) {
	if len(buffer) < 2 || buffer[0] != 0xFF || buffer[1] != 0xD8 {
		return nil, false
	}
	endMarker := bytes.LastIndex(buffer, []byte{0xFF, 0xD9})
	if endMarker <= 2 {
		return nil, false
	}
	return buffer[:endMarker+2], true
}

func cameraOffersJPEGAt(devicePath string, width, height uint32) bool {
	return width > 0 && height > 0 && deviceSupportsMJPEGSize(devicePath, width, height)
}

// nativeJPEGCaptureSize reports whether a camera node the agent opens itself
// should be captured as JPEG for this request, and at what size.
func nativeJPEGCaptureSize(devicePath string, req *agentpb.StreamVideoRequest) (width, height uint32, ok bool) {
	if req.GetCodec() == agentpb.VideoCodec_VIDEO_CODEC_RAW || thermalCaptureProfile(devicePath) != nil {
		return 0, 0, false
	}
	// A default request is sized at the module's stacked metadata mode when it
	// has one, which leaves that module to the GStreamer planner.
	const stackedRowsCroppable = true
	width, height = resolveV4L2CaptureSize(devicePath, req, nil, stackedRowsCroppable)
	if !cameraOffersJPEGAt(devicePath, width, height) {
		return 0, 0, false
	}
	return width, height, true
}

type nativeJPEGFrames struct {
	broadcast func([]byte, frameTimestamp, agentpb.VideoCodec) bool
	logger    *zap.Logger
	device    string

	delivered      bool
	unusable       int
	unusableReason string
}

func (f *nativeJPEGFrames) emit(buffer []byte, stamp frameTimestamp) bool {
	frame, isJPEG := jpegFrameInBuffer(buffer)
	switch {
	case stamp.nativeFlags&v4l2BufFlagError != 0:
		return f.drop("the driver flagged the buffer as damaged", len(buffer))
	case !stamp.auAligned:
		return f.drop("the buffer exceeds the frame size limit", len(buffer))
	case !isJPEG:
		return f.drop("the buffer is not a JPEG image", len(buffer))
	}
	f.delivered = true
	return f.broadcast(frame, stamp, videoCodecMJPEG)
}

func (f *nativeJPEGFrames) drop(reason string, bufferBytes int) (keepCapturing bool) {
	f.unusableReason = reason
	if f.unusable++; f.unusable == 1 {
		f.logger.Warn("dropping unusable JPEG camera buffers",
			zap.String("device", f.device), zap.String("reason", reason), zap.Int("bytes", bufferBytes))
	}
	return !f.gaveUp()
}

func (f *nativeJPEGFrames) gaveUp() bool {
	return !f.delivered && f.unusable >= nativeJPEGUnusableBuffersBeforeNotSupported
}

func (f *nativeJPEGFrames) captureResult(captureErr error) error {
	if captureErr == nil && f.gaveUp() {
		return nativeJPEGNotSupported{msg: fmt.Sprintf("the first %d buffers from %s held no usable JPEG frame: %s",
			f.unusable, f.device, f.unusableReason)}
	}
	return captureErr
}

func nativeJPEGCaptureConfig(width, height, framerate uint32) v4l2CaptureConfig {
	return v4l2CaptureConfig{
		pixelFormat:      v4l2PixFmtMJPEG,
		width:            width,
		height:           height,
		exactSize:        true,
		framerate:        framerate,
		setFrameInterval: true,
		formatUnsupported: func(msg string) error {
			return nativeJPEGNotSupported{msg: msg}
		},
	}
}

func (s *VideoService) captureNativeJPEG(ctx context.Context, broadcast func([]byte, frameTimestamp, agentpb.VideoCodec) bool, path string, width, height, framerate uint32) error {
	if width == 0 || height == 0 {
		return status.Errorf(codes.Internal, "native JPEG capture of %s needs the size nativeJPEGCaptureSize chose, got %dx%d", path, width, height)
	}
	frames := &nativeJPEGFrames{broadcast: broadcast, logger: s.logger, device: path}
	return frames.captureResult(s.captureV4L2Native(ctx, frames.emit, path, nativeJPEGCaptureConfig(width, height, framerate)))
}
