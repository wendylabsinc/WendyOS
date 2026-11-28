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

// What the frame gate tells apart in the byte after an FF: the markers it
// acts on, and the zero that makes the FF before it a byte of entropy-coded
// data.
const (
	jpegMarkerPrefix           = 0xFF
	jpegStuffedFF              = 0x00
	jpegTemporary              = 0x01
	jpegHuffmanTables          = 0xC4
	jpegArithmeticConditioning = 0xCC
	jpegFirstRestart           = 0xD0
	jpegLastRestart            = 0xD7
	jpegStartOfImage           = 0xD8
	jpegEndOfImage             = 0xD9
	jpegStartOfScan            = 0xDA
	jpegQuantizationTables     = 0xDB
	jpegNumberOfLines          = 0xDC
	jpegRestartInterval        = 0xDD
	jpegFirstApplication       = 0xE0
	jpegLastApplication        = 0xEF
	jpegComment                = 0xFE
	jpegSegmentLengthBytes     = 2
)

// jpegMarkerEndsScan reports the markers that can follow the entropy-coded
// data of a scan: the end of the image, a further scan, and the segments
// allowed between scans.
func jpegMarkerEndsScan(marker byte) bool {
	switch marker {
	case jpegEndOfImage, jpegStartOfScan, jpegHuffmanTables, jpegArithmeticConditioning,
		jpegQuantizationTables, jpegNumberOfLines, jpegRestartInterval, jpegComment:
		return true
	}
	return marker >= jpegFirstApplication && marker <= jpegLastApplication
}

// jpegFrameInBuffer returns the one whole JPEG image a capture buffer starts
// with. Webcams follow the end marker with zeros, FF bytes or vendor data,
// which are left out.
//
// The gate is there to catch a frame cut short, so the end of the image is
// found by reading it, not by searching for an end marker: an application
// segment may hold a thumbnail, a whole image with an end marker of its own.
// Segments are stepped over by their lengths, and the image ends at the end
// marker that follows a scan. It is not there to judge headers: bytes between
// segments that are no marker, a repeated start marker and an end marker
// before any scan are stepped over, and an FF in a scan that no legal marker
// follows is data. A segment whose length is under two or runs past the
// buffer is a cut or not an image.
func jpegFrameInBuffer(buffer []byte) (frame []byte, ok bool) {
	if len(buffer) < 2 || buffer[0] != jpegMarkerPrefix || buffer[1] != jpegStartOfImage {
		return nil, false
	}
	scans := 0
	for at := 2; at+1 < len(buffer); {
		if buffer[at] != jpegMarkerPrefix {
			at++
			continue
		}
		marker := buffer[at+1]
		switch {
		case marker == jpegMarkerPrefix || marker == jpegStuffedFF:
			at++
		case marker == jpegEndOfImage && scans > 0:
			return buffer[:at+2], true
		case marker == jpegEndOfImage || marker == jpegStartOfImage || marker == jpegTemporary ||
			(marker >= jpegFirstRestart && marker <= jpegLastRestart):
			at += 2
		default:
			if at+2+jpegSegmentLengthBytes > len(buffer) {
				return nil, false
			}
			length := int(buffer[at+2])<<8 | int(buffer[at+3])
			if length < jpegSegmentLengthBytes || at+2+length > len(buffer) {
				return nil, false
			}
			at += 2 + length
			if marker == jpegStartOfScan {
				scans++
				at = jpegScanEnd(buffer, at)
			}
		}
	}
	return nil, false
}

// jpegScanEnd returns where the entropy-coded data starting at from ends: at
// the first marker that can follow a scan, or at the end of the buffer when
// the scan is cut short. A start marker in a scan is another image begun
// before this one ended, which is a cut too.
func jpegScanEnd(buffer []byte, from int) int {
	for at := from; ; {
		next := bytes.IndexByte(buffer[at:], jpegMarkerPrefix)
		if next < 0 || at+next+1 >= len(buffer) {
			return len(buffer)
		}
		at += next
		switch following := buffer[at+1]; {
		case following == jpegMarkerPrefix:
			at++
		case following == jpegStartOfImage:
			return len(buffer)
		case jpegMarkerEndsScan(following):
			return at
		default:
			at += 2
		}
	}
}

// jpegImageFollows reports that the bytes after a frame taken from buffer,
// past any zero or FF padding, are a whole image of their own.
func jpegImageFollows(buffer, frame []byte) bool {
	at := len(frame)
	for at+1 < len(buffer) && (buffer[at] == 0x00 || (buffer[at] == jpegMarkerPrefix && buffer[at+1] != jpegStartOfImage)) {
		at++
	}
	_, whole := jpegFrameInBuffer(buffer[at:])
	return whole
}

func cameraOffersJPEGAt(devicePath string, width, height uint32) bool {
	return width > 0 && height > 0 && deviceSupportsMJPEGSize(devicePath, width, height)
}

// A JPEG frame above this size can exceed maxFrameBytes, which the hub does
// not distribute, so larger captures stay with GStreamer.
const nativeJPEGMaxWidth, nativeJPEGMaxHeight = 1920, 1080

func nativeJPEGCapture(devicePath string, req *agentpb.StreamVideoRequest) (width, height uint32, declined string) {
	switch {
	case req.GetCodec() == agentpb.VideoCodec_VIDEO_CODEC_RAW:
		return 0, 0, "the camera was started for a raw frame request, which captures it uncompressed"
	case thermalCaptureProfile(devicePath) != nil:
		return 0, 0, "thermal cameras are captured uncompressed, for their measurements"
	}
	// A default request is sized at the module's stacked metadata mode when it
	// has one, which leaves that module to the GStreamer planner.
	const stackedRowsCroppable = true
	width, height = resolveV4L2CaptureSize(devicePath, req, nil, stackedRowsCroppable)
	switch {
	case width == 0 || height == 0:
		return 0, 0, "the camera reports no capture size"
	case width > nativeJPEGMaxWidth || height > nativeJPEGMaxHeight:
		return 0, 0, fmt.Sprintf("%dx%d is above the %dx%d limit for capturing JPEG frames whole", width, height, nativeJPEGMaxWidth, nativeJPEGMaxHeight)
	case !cameraOffersJPEGAt(devicePath, width, height):
		return 0, 0, fmt.Sprintf("the camera offers no JPEG at %dx%d", width, height)
	}
	return width, height, ""
}

func nativeJPEGCaptureSize(devicePath string, req *agentpb.StreamVideoRequest) (width, height uint32, ok bool) {
	width, height, declined := nativeJPEGCapture(devicePath, req)
	return width, height, declined == ""
}

func nativeJPEGFrameRate(devicePath string, req *agentpb.StreamVideoRequest, width, height uint32) uint32 {
	if rate := req.GetFramerate(); rate > 0 {
		return rate
	}
	return highestFrameRateFor(devicePath, v4l2PixFmtMJPEG, width, height)
}

type nativeJPEGFrames struct {
	broadcast func([]byte, frameTimestamp, agentpb.VideoCodec) bool
	logger    *zap.Logger
	device    string

	delivered         bool
	unusable          int
	unusableReason    string
	warnedOfTwoImages bool
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
	if !f.warnedOfTwoImages && jpegImageFollows(buffer, frame) {
		f.warnedOfTwoImages = true
		f.logger.Warn("camera buffer holds a second JPEG image after the first; only the first is handed on",
			zap.String("device", f.device), zap.Int("first_image_bytes", len(frame)), zap.Int("buffer_bytes", len(buffer)))
	}
	f.delivered = true
	stamp.native = true
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
		stallLimit:       cameraIdleTimeout,
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
	return frames.captureResult(s.v4l2Capture(ctx, frames.emit, path, nativeJPEGCaptureConfig(width, height, framerate)))
}
