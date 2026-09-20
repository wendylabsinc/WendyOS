package services

// Raw frame tap: uncompressed capture frames for the subscribers that ask.
//
// The hub hands every subscriber the same encoded stream, because a picture is
// what a viewer wants. A thermal module is the case where it is not. The TOPDON
// TC001 (an InfiRay core) stacks a block of 16-bit per-pixel temperatures onto
// the picture rows of its 512x484 YUYV mode, and H.264 destroys those values on
// the way to the client — a hotspot detector, a thermography readout, a fire
// alarm, all of them need the bytes the camera produced. Until now the only way
// to get them was to open /dev/videoN directly, which locks the companion app
// and `camera view` out of that camera for as long as the analytic app runs
// (one process per V4L2 node, kernel rule).
//
// So the producer's GStreamer pipeline tees the raw capture to a second pipe and
// the hub delivers it, unencoded, to subscribers that set
// StreamVideoRequest.raw. Viewers on the same camera keep their H.264. One
// capture, two audiences.
//
// Raw is offered only where it costs nothing to promise: a v4l2src pipeline that
// already captures raw YUYV (no MJPEG, no native H.264, not shared through
// PipeWire) at a size the device advertises, for frames that fit a default gRPC
// message. Everywhere else a raw subscriber is told so — FailedPrecondition with
// reason RAW_UNAVAILABLE — instead of being left on a stream that never delivers.
// A subscriber waiting silently for frames that will never come is the exact
// failure this exists to remove, so it is not allowed to happen here either.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"

	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const (
	// rawTapFD is the child fd the pipeline's raw branch writes to: 0-2 are stdio,
	// so the first ExtraFiles slot is 3.
	rawTapFD = 3

	// rawTeeName names the tee element so the raw branch can be attached to it in
	// gst-launch syntax ("rawtap. ! ...").
	rawTeeName = "rawtap"

	// rawTapQueue drops the oldest raw frame rather than stalling. A tee blocks on
	// its slowest branch, so a raw reader that fell behind would otherwise freeze
	// the encoded branch — and a viewer's picture must never depend on how fast an
	// analytic subscriber reads.
	rawTapQueue = "queue max-size-buffers=2 max-size-bytes=0 max-size-time=0 leaky=downstream"

	// maxRawFrameBytes bounds a raw frame so that every client can receive it:
	// grpc-go's default per-message receive limit is 4 MiB, and one raw frame is
	// one message. A camera whose raw frame is larger is simply not offered raw,
	// rather than offered a stream the default client rejects on the first frame.
	// A 1280x720 YUYV frame (1.8 MiB) fits; 1080p (4.0 MiB) does not.
	maxRawFrameBytes = 4*1024*1024 - 64*1024

	// yuyvBytesPerPixel: YUYV packs two pixels in four bytes.
	yuyvBytesPerPixel = 2
	fourccYUYV        = "YUYV"

	// A stacked thermal mode is the sensor's picture plus a block of metadata rows
	// (100 on the TC001: 96 rows of 16-bit temperatures and 4 of header). These
	// bound how tall that block may be for a mode to be read as stacked.
	minStackedRows = 16
	maxStackedRows = 128
)

// thermalMetadataRowsOnTop is where the stacked block sits relative to the
// picture. Observed on wendy-box-theta and ccr1: the TC001's 512x484 mode rendered
// its extra rows as a stripe across the TOP of the frame (bestDefaultFrameSize).
// A var so a module that stacks them underneath can be accommodated in one line.
var thermalMetadataRowsOnTop = true

// rawTapState tracks whether a hub's producer offers raw frames.
type rawTapState int

const (
	rawUndecided rawTapState = iota
	rawAvailable
	rawUnavailable
)

// rawSink is what a producer needs from its hub to hand out raw frames: whether
// anyone is listening (to skip the copy when not), a way to publish, and a way
// to record the decision either way. deviceHub implements it; producers that
// can never offer raw get noRawSink.
type rawSink interface {
	wantRaw() bool
	publishRaw(data []byte, tsNs uint64, format *agentpb.RawFormat) bool
	rawOffered(format *agentpb.RawFormat)
	// rawNotOffered takes the reason as a function: a refusal that has to
	// probe the device is computed only if a raw subscriber ever asks.
	rawNotOffered(reason func() string)
}

// noRawSink is the sink for producers whose raw decision was made by the caller.
type noRawSink struct{}

func (noRawSink) wantRaw() bool                                      { return false }
func (noRawSink) publishRaw([]byte, uint64, *agentpb.RawFormat) bool { return true }
func (noRawSink) rawOffered(*agentpb.RawFormat)                      {}
func (noRawSink) rawNotOffered(func() string)                        {}

// errRawUnavailable is the answer a raw subscriber gets from a camera that has
// no raw frames to give. Machine-readable reason so the CLI can name the fix.
func errRawUnavailable(reason string) error {
	return streamreason.New(codes.FailedPrecondition,
		"raw frames are not available for this camera: "+reason,
		streamreason.RawUnavailable, nil)
}

// requestsDeviceDefault reports whether a request names no size or rate at all —
// the shape a bare `camera view` and the companion app send.
func requestsDeviceDefault(req *agentpb.StreamVideoRequest) bool {
	return req.GetWidth() == 0 && req.GetHeight() == 0 && req.GetFramerate() == 0
}

// rawPixelFormat is a capture format the tap can offer. Raw was originally
// gated on YUYV alone, which is what the TOPDON happens to use -- but the
// promise ("the bytes the camera produced") has nothing to do with that format,
// and RawFormat already carries a fourcc to describe whatever arrives. A
// thermal camera exposing Y16, or a mono sensor exposing GREY, was refused for
// no reason beyond the table it was missing from.
//
// A format cannot simply be passed through, though: the capture caps must NAME
// it to GStreamer, so each entry pairs the V4L2 fourcc with its caps spelling.
// That is the real boundary -- not the fourcc, the format we can build a
// pipeline for.
type rawPixelFormat struct {
	fourcc    string // what RawFormat reports, and what v4l2 calls it
	v4l2      uint32 // V4L2_PIX_FMT_*
	gstFormat string // video/x-raw,format=<this>
	// bytesPerLine for one row. Packed formats only: a planar format's stride
	// is not one number, and describing it as one would be a lie a subscriber
	// would then decode against.
	bytesPerLine func(width uint32) uint32
}

func twoBytesPerPixel(w uint32) uint32 { return w * 2 }
func oneBytePerPixel(w uint32) uint32  { return w }

// rawPixelFormats is tried in order, so the first entry is preferred when a
// camera advertises several. YUYV stays first: it is what every UVC webcam
// offers and what the thermal modules use, so the common path is unchanged.
//
// Deliberately packed formats only. NV12 and other planar layouts have a
// per-plane stride that bytes_per_line cannot express, and Bayer needs
// video/x-bayer rather than video/x-raw -- a different caps type, not just a
// different name. Both are refusals with a reason, not silent omissions.
var rawPixelFormats = []rawPixelFormat{
	{fourcc: "YUYV", v4l2: v4l2PixFmtYUYV, gstFormat: "YUY2", bytesPerLine: twoBytesPerPixel},
	{fourcc: "UYVY", v4l2: v4l2PixFmtUYVY, gstFormat: "UYVY", bytesPerLine: twoBytesPerPixel},
	// Y16 is the one that matters for thermal: plenty of cores expose 16-bit
	// per-pixel data directly rather than stacking it onto a picture.
	{fourcc: "Y16 ", v4l2: v4l2PixFmtY16, gstFormat: "GRAY16_LE", bytesPerLine: twoBytesPerPixel},
	{fourcc: "GREY", v4l2: v4l2PixFmtGrey, gstFormat: "GRAY8", bytesPerLine: oneBytePerPixel},
}

// Z16 -- 16-bit depth, what a RealSense reports on its depth node -- is
// deliberately NOT in the table above, and it is the one exclusion that is not
// about packing. It IS packed and it IS sixteen bits per pixel, so the layout
// RawFormat describes would be exact. Two other things stop it:
//
//   - THIS pipeline cannot capture it. Each entry pairs a fourcc with a caps
//     spelling because v4l2src selects the V4L2 format from the caps, and
//     gst-plugins-good's v4l2 format table has no V4L2_PIX_FMT_Z16 entry at
//     all (checked against branches 1.24 and main). GRAY16_LE maps back to
//     Y16/Y16_BE, which a depth node does not advertise, so those caps would
//     fail to negotiate rather than deliver depth.
//
//     Note carefully what that does NOT say. The agent is not GStreamer-only
//     at the device layer -- streamV4L2Native (video_service.go) already opens
//     a node, sets a format and pumps mmap buffers itself, and generalising it
//     past its hardcoded H.264 fourcc would capture Z16 fine. GStreamer bounds
//     THIS table, not the agent.
//   - A depth frame is meaningless without its scale, and that is what actually
//     blocks it. Z16 counts device units; metres are units times a per-device
//     depth scale. On a RealSense that scale is RS2_OPTION_DEPTH_UNITS -- a
//     vendor XU control, not a V4L2 one, that differs by model (a D405 is not a
//     D435) and is WRITABLE, so a visual preset can change it under you.
//     RawFormat carries width, height, fourcc and stride -- no scale -- so a
//     subscriber would get uint16 in unknown units with no way to notice.
//     Guessing 1 mm is exactly the silent degradation this file exists to
//     prevent, and it would be wrong by a factor of ten on a D405.
//   - Colour and depth are separate nodes, so a second tap is a second capture:
//     two clocks, no frame pairing, and no extrinsics to align one to the
//     other. Alignment is not something a subscriber can reconstruct from that.
//
// So depth belongs to a source that owns the sensor, pairs the two planes and
// reports intrinsics and scale alongside them. That is
// WendyCalibratedFrameService (calibrated_frame_service.go) and
// specs/2026-09-20-calibrated-frame-sensor-source-design.md, not another row
// here. A direct-V4L2 Z16 capture path is still the right way to serve a depth
// camera the vendor SDK does not cover -- but it belongs behind that contract,
// as a framesource.Source emitting SENSOR_NATIVE depth, not as a row in a table
// whose message cannot say what its numbers mean.

// advertisesDepth reports whether the device offers Z16 modes: at exactly this
// size when one was chosen, anywhere when none was. It exists only to make a
// refusal specific, so it is called on the refusal path and nowhere else.
func advertisesDepth(devicePath string, width, height uint32) bool {
	if width == 0 || height == 0 {
		return len(enumerateRawFrameSizes(devicePath, v4l2PixFmtZ16)) > 0
	}
	return deviceSupportsRawSize(devicePath, v4l2PixFmtZ16, width, height)
}

// rawWhyDepthNode names what the node IS rather than what it lacks. Telling the
// owner of a depth sensor that their camera advertises none of YUYV/UYVY/Y16/
// GREY is true and useless: no setting on that node will ever produce one, and
// the message sends them to debug a camera working exactly as designed.
func rawWhyDepthNode() string {
	return "camera is a depth node (Z16); the raw tap captures picture formats (" +
		rawFormatNames() + ") and does not carry depth"
}

// rawWhyNoCaptureSize is the refusal for a camera no capture size could be
// picked for. "Advertises no discrete frame size" is true of a driver without
// VIDIOC_ENUM_FRAMESIZES -- and false of a depth node, which advertises plenty,
// none of them in a format bestDefaultFrameSize looks for (it enumerates YUYV
// and MJPEG only).
func rawWhyNoCaptureSize(devicePath string) string {
	if advertisesDepth(devicePath, 0, 0) {
		return rawWhyDepthNode()
	}
	return "camera advertises no discrete frame size to capture raw frames at"
}

// rawWhyNoFormat is the refusal for a size the tap has no format for. The depth
// case only ever REPLACES this refusal, never creates one: a camera offering a
// picture format alongside depth still gets its raw frames.
func rawWhyNoFormat(devicePath string, width, height uint32) string {
	if advertisesDepth(devicePath, width, height) {
		return rawWhyDepthNode()
	}
	return fmt.Sprintf("camera advertises none of %s at %dx%d; raw frames are not offered",
		rawFormatNames(), width, height)
}

// rawFormatFor picks the format the tap will capture in: the first entry the
// device advertises at exactly this size. Returns nil when the camera offers
// none of them at that size, which is a refusal with a reason rather than a
// silent fallback to a format the pipeline is not actually producing.
func rawFormatFor(devicePath string, width, height uint32) *rawPixelFormat {
	for i := range rawPixelFormats {
		f := &rawPixelFormats[i]
		if deviceSupportsRawSize(devicePath, f.v4l2, width, height) {
			return f
		}
	}
	return nil
}

// rawFormatNames lists what the tap can offer, for the refusal message: telling
// someone their camera is unsupported is only useful alongside what is.
func rawFormatNames() string {
	names := make([]string, 0, len(rawPixelFormats))
	for _, f := range rawPixelFormats {
		names = append(names, strings.TrimSpace(f.fourcc))
	}
	return strings.Join(names, "/")
}

// enumerateYUYVFrameSizes lists the discrete YUYV modes a device advertises.
// Behind a var so pipeline-construction tests can describe a camera without a
// V4L2 node (same spirit as deviceSupportsMJPEGSize). Read-only open, so it
// answers while the camera is streaming.
var enumerateRawFrameSizes = func(path string, pixfmt uint32) [][2]uint32 {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	defer unix.Close(fd) //nolint:errcheck
	var sizes [][2]uint32
	for index := uint32(0); index < 64; index++ {
		fse := v4l2FrmSizeEnum{Index: index, PixelFormat: pixfmt}
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocEnumFramesizes,
			uintptr(unsafe.Pointer(&fse))); errno != 0 {
			break // EINVAL marks the end of the list
		}
		if fse.Type != v4l2FrmsizeTypeDiscrete {
			break
		}
		sizes = append(sizes, [2]uint32{fse.Width, fse.Height})
	}
	return sizes
}

// enumerateYUYVFrameSizes is the YUYV case of the above. The stacked-mode
// geometry below reasons about the picture modes a thermal camera advertises,
// and those are YUYV on every module measured.
func enumerateYUYVFrameSizes(path string) [][2]uint32 {
	return enumerateRawFrameSizes(path, v4l2PixFmtYUYV)
}

// deviceSupportsRawSize reports whether the device advertises this format at
// exactly width x height — the precondition for pinning it on the capture and
// promising that layout to raw subscribers.
var deviceSupportsRawSize = func(path string, pixfmt, width, height uint32) bool {
	for _, m := range enumerateRawFrameSizes(path, pixfmt) {
		if m[0] == width && m[1] == height {
			return true
		}
	}
	return false
}

// stackedMetadataRows returns how many rows of a captured mode are metadata
// rather than picture, or 0 for an ordinary mode.
//
// A stacked mode has a non-standard aspect (512x484 is 1.058) and sits a short
// block of rows above a standard-aspect mode of the same width that the device
// also advertises (512x384, 4:3). Both conditions are required: 640x480 also
// has a 640x360 sibling 120 rows shorter, and it is a plain 4:3 picture that
// must never be cropped.
func stackedMetadataRows(devicePath string, w, h uint32) uint32 {
	if w == 0 || h == 0 || hasStandardAspect(w, h) {
		return 0
	}
	var picture uint32
	for _, m := range enumerateYUYVFrameSizes(devicePath) {
		if m[0] != w || m[1] >= h || !hasStandardAspect(m[0], m[1]) {
			continue
		}
		if diff := h - m[1]; diff < minStackedRows || diff > maxStackedRows {
			continue
		}
		if m[1] > picture {
			picture = m[1]
		}
	}
	if picture == 0 {
		return 0
	}
	return h - picture
}

// stackedModeAbove returns the stacked variant of a plain picture mode — the same
// width, a short block of rows taller, non-standard aspect — that the device also
// advertises, or (0, 0) when there is none. The inverse of stackedMetadataRows:
// that one asks "how much of this tall mode is metadata", this asks "does this
// picture mode have a tall sibling carrying metadata". The nearest sibling wins.
func stackedModeAbove(devicePath string, w, h uint32) (uint32, uint32) {
	if w == 0 || h == 0 || !hasStandardAspect(w, h) {
		return 0, 0
	}
	var bestH uint32
	for _, m := range enumerateYUYVFrameSizes(devicePath) {
		if m[0] != w || m[1] <= h || hasStandardAspect(m[0], m[1]) {
			continue
		}
		if diff := m[1] - h; diff < minStackedRows || diff > maxStackedRows {
			continue
		}
		if bestH == 0 || m[1] < bestH {
			bestH = m[1]
		}
	}
	if bestH == 0 {
		return 0, 0
	}
	return w, bestH
}

// pumpRawTap reads whole frames off the raw pipe and hands them to the hub while
// anyone wants them. It always drains: a tee whose branch stops being read stalls
// the encoder branch with it, so frames nobody wants are read and dropped.
//
// Frames are fixed-size (bytes_per_line x height), so the pipe needs no framing
// protocol; a short read means the pipeline ended. Timestamps are taken here
// rather than from GStreamer: the raw branch carries no metadata, and wall-clock
// at arrival is what the encoded path reports too.
func (s *VideoService) pumpRawTap(ctx context.Context, r *os.File, format *agentpb.RawFormat, sink rawSink, device string) {
	defer r.Close() //nolint:errcheck
	frameBytes := int(format.GetBytesPerLine()) * int(format.GetHeight())
	if frameBytes <= 0 || frameBytes > maxRawFrameBytes {
		s.logger.Warn("raw frame tap has an unusable frame size; not reading it",
			zap.String("device", device), zap.Int("frame_bytes", frameBytes))
		return
	}
	buf := make([]byte, frameBytes)
	var frames, delivered uint64
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			if ctx.Err() == nil && err != io.EOF {
				s.logger.Debug("raw frame tap ended", zap.String("device", device), zap.Error(err))
			}
			s.logger.Debug("raw frame tap closed", zap.String("device", device),
				zap.Uint64("frames", frames), zap.Uint64("delivered", delivered))
			return
		}
		frames++
		if !sink.wantRaw() {
			continue // nobody listening: keep the tee flowing, spend nothing on a copy
		}
		data := make([]byte, frameBytes)
		copy(data, buf)
		if !sink.publishRaw(data, uint64(time.Now().UnixNano()), format) {
			return // hub has no subscribers of any kind; the producer is stopping
		}
		delivered++
	}
}

// describeRawFormat renders a RawFormat for logs and CLI output.
func describeRawFormat(f *agentpb.RawFormat) string {
	if f == nil {
		return "encoded"
	}
	return fmt.Sprintf("%dx%d %s (%d bytes/line, %d bytes/frame)",
		f.GetWidth(), f.GetHeight(), f.GetFourcc(), f.GetBytesPerLine(),
		f.GetBytesPerLine()*f.GetHeight())
}
