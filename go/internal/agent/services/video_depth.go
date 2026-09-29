package services

// Native depth capture: Z16 frames from a depth camera's Video for Linux 2
// (V4L2) node, read by the agent itself.
//
// A UVC depth camera such as the Intel RealSense D400 exposes its depth stream
// as a separate node whose only useful format is 'Z16 ': one little-endian
// 16-bit depth value per pixel. The raw tap cannot serve it, because the tap is
// a branch of a GStreamer pipeline and GStreamer's v4l2src has no mapping for
// Z16 at all (it is absent from the gst-plugins-good v4l2 format table at
// 1.20.0, 1.22.12, 1.24.12, 1.26.0 and main), so no caps can name it. Nor is there a picture to
// encode: an H.264 stream of depth values quantised to 8-bit luma would be
// worse than useless to a viewer and destroys the values an analytic consumer
// needs.
//
// So a depth node gets a raw-only hub. The producer negotiates Z16 with
// VIDIOC_S_FMT, streams mmap buffers through the same queue and dequeue loop
// the native H.264 path uses (video_v4l2_mmap.go), and publishes every frame on
// the raw plane through publishRaw, which stamps it with the canonical
// boot-clock receipt and the device's raw sample counter. Encoded subscribers
// are turned away with a reason instead of being left waiting for frames that
// will never come.

import (
	"context"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const (
	// fourccZ16 is what RawFormat reports for a depth frame.
	fourccZ16 = "Z16 "

	// depthCaptureBuffers is how many mmap buffers the depth producer asks for.
	// More than the H.264 path's two: a depth frame is large and uncompressed,
	// and the extra buffers let the driver keep filling while one frame is being
	// copied out.
	depthCaptureBuffers = 4

	// maxPixelFormatIndex stops a VIDIOC_ENUM_FMT walk on a driver that never
	// answers EINVAL.
	maxPixelFormatIndex = 64
)

// depthEncodedReason is what an encoded subscriber to a depth node is told.
const depthEncodedReason = "camera is a depth sensor (Z16) read natively by the agent; " +
	"it serves raw depth frames only, so request raw frames"

// errDepthCaptureLinuxOnly is what the native depth producer returns on a
// platform without V4L2.
var errDepthCaptureLinuxOnly = status.Error(codes.Unimplemented, "native depth capture is Linux only")

// errEncodedUnavailable is the answer an encoded subscriber gets from a hub
// that only has raw frames to give.
func errEncodedUnavailable(reason string) error {
	return status.Error(codes.FailedPrecondition, "encoded video is not available for this camera: "+reason)
}

// v4l2FmtDesc matches struct v4l2_fmtdesc (64 bytes).
type v4l2FmtDesc struct {
	Index       uint32
	Type        uint32
	Flags       uint32
	Description [32]uint8
	PixelFormat uint32
	MbusCode    uint32
	Reserved    [3]uint32
}

var (
	_ [64 - unsafe.Sizeof(v4l2FmtDesc{})]byte
	_ [unsafe.Sizeof(v4l2FmtDesc{}) - 64]byte
)

// devicePixelFormats lists the single-planar capture formats a node advertises
// through VIDIOC_ENUM_FMT. Read-only open, so it answers while the node is
// streaming. Behind a var so tests can describe a node without one, in the
// same spirit as enumerateRawFrameSizes.
var devicePixelFormats = func(path string) []uint32 {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	defer unix.Close(fd) //nolint:errcheck
	var formats []uint32
	for index := uint32(0); index < maxPixelFormatIndex; index++ {
		desc := v4l2FmtDesc{Index: index, Type: v4l2BufTypeVideoCapture}
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocEnumFmt,
			uintptr(unsafe.Pointer(&desc))); errno != 0 {
			break // EINVAL marks the end of the list
		}
		formats = append(formats, desc.PixelFormat)
	}
	return formats
}

// isDepthNode reports whether a node is a depth camera: it advertises Z16.
func isDepthNode(path string) bool {
	for _, f := range devicePixelFormats(path) {
		if f == v4l2PixFmtZ16 {
			return true
		}
	}
	return false
}

// depthFrameBytes is the size of one packed Z16 frame.
func depthFrameBytes(width, height uint32) uint64 {
	return uint64(twoBytesPerPixel(width)) * uint64(height)
}

// depthFrameSize picks the capture size for a depth node: the largest discrete
// Z16 mode whose frame fits maxRawFrameBytes, so every frame fits one default
// gRPC message as the raw tap promises. (0, 0) when there is none.
func depthFrameSize(path string) (uint32, uint32) {
	var bestW, bestH uint32
	for _, m := range enumerateRawFrameSizes(path, v4l2PixFmtZ16) {
		if depthFrameBytes(m[0], m[1]) > maxRawFrameBytes {
			continue
		}
		if uint64(m[0])*uint64(m[1]) > uint64(bestW)*uint64(bestH) {
			bestW, bestH = m[0], m[1]
		}
	}
	return bestW, bestH
}

// captureDepthFrames streams Z16 frames of exactly width x height from a depth
// node until ctx ends, deliver returns false, or the device fails. offered is
// called once, after the format is negotiated and before the first frame, with
// the row stride the driver reports. deliver receives each whole frame; the
// slice may alias driver memory and is valid only for the duration of the call.
//
// Behind a var so the producer can be driven by a fake frame source in tests.
// The native implementation is Linux only (video_depth_linux.go); elsewhere it
// returns errDepthCaptureLinuxOnly.
var captureDepthFrames = captureDepthFramesNative

// runDepthProducer feeds a raw-only hub from a depth node. It runs on the
// producer goroutine in place of the encoded capture paths.
func (s *VideoService) runDepthProducer(ctx context.Context, h *deviceHub, path string, req *agentpb.StreamVideoRequest) error {
	h.encodedNotOffered(depthEncodedReason)

	width, height := req.GetWidth(), req.GetHeight()
	if width == 0 || height == 0 {
		width, height = depthFrameSize(path)
		if width == 0 || height == 0 {
			reason := fmt.Sprintf("depth camera advertises no discrete Z16 frame size within the %d-byte raw frame limit", maxRawFrameBytes)
			h.rawNotOffered(reason)
			return errRawUnavailable(reason)
		}
	} else {
		if !deviceSupportsRawSize(path, v4l2PixFmtZ16, width, height) {
			reason := fmt.Sprintf("depth camera does not advertise Z16 at %dx%d", width, height)
			h.rawNotOffered(reason)
			return errRawUnavailable(reason)
		}
		if depthFrameBytes(width, height) > maxRawFrameBytes {
			reason := fmt.Sprintf("a %dx%d Z16 frame exceeds the %d-byte raw frame limit", width, height, maxRawFrameBytes)
			h.rawNotOffered(reason)
			return errRawUnavailable(reason)
		}
	}

	var format *agentpb.RawFormat
	offered := func(bytesPerLine uint32) {
		format = &agentpb.RawFormat{Width: width, Height: height, Fourcc: fourccZ16, BytesPerLine: bytesPerLine}
		h.rawOffered(format)
	}
	deliver := func(frame []byte) bool {
		if !h.wantRaw() {
			return true // nobody listening: keep the queue moving, spend nothing on a copy
		}
		data := make([]byte, len(frame))
		copy(data, frame)
		return h.publishRaw(data, uint64(time.Now().UnixNano()), format)
	}
	return captureDepthFrames(ctx, s, path, width, height, offered, deliver)
}
