package services

import (
	"context"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type nativeJPEGNotSupported struct{ msg string }

func (e nativeJPEGNotSupported) Error() string { return e.msg }

// Some webcams fill the rest of the buffer with zero bytes after the end
// marker.
func jpegWithoutPadding(data []byte) (jpeg []byte, ok bool) {
	end := len(data)
	for end > 0 && data[end-1] == 0x00 {
		end--
	}
	if end < 4 ||
		data[0] != 0xFF || data[1] != 0xD8 ||
		data[end-2] != 0xFF || data[end-1] != 0xD9 {
		return nil, false
	}
	return data[:end], true
}

func cameraOffersJPEGAt(devicePath string, width, height uint32) bool {
	return width > 0 && height > 0 && deviceSupportsMJPEGSize(devicePath, width, height)
}

// nativeJPEGCaptureSize reports whether a camera node the agent opens itself
// should be captured as JPEG for this request, and at what size.
func nativeJPEGCaptureSize(devicePath string, req *agentpb.StreamVideoRequest) (width, height uint32, ok bool) {
	// A default request on a module with a stacked metadata mode resolves to
	// that mode, as it does in the GStreamer planner when it can crop the
	// rows, so such a camera keeps its raw capture.
	const stackedRowsCroppable = true
	width, height = resolveV4L2CaptureSize(devicePath, req, thermalCaptureProfile(devicePath), stackedRowsCroppable)
	if !cameraOffersJPEGAt(devicePath, width, height) {
		return 0, 0, false
	}
	return width, height, true
}

func nativeJPEGEmit(broadcast func([]byte, frameTimestamp, agentpb.VideoCodec) bool) func([]byte, frameTimestamp) bool {
	return func(data []byte, stamp frameTimestamp) bool {
		jpeg, ok := jpegWithoutPadding(data)
		if !stamp.auAligned || !ok {
			return true
		}
		return broadcast(jpeg, stamp, videoCodecMJPEG)
	}
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
	return s.captureV4L2Native(ctx, nativeJPEGEmit(broadcast), path, nativeJPEGCaptureConfig(width, height, framerate))
}
