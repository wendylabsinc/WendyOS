package services

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// sharedMode is the one format an app's agent-fed camera node declares. It
// always comes from shareDecisionFor, so the node declares exactly what the
// hub subscription of its Format delivers.
type sharedMode struct {
	Width, Height uint32
	Fourcc        string
	BytesPerLine  uint32
	FPS           uint32
	// Format is the entitlement format the node serves (appconfig.CameraFormats).
	Format string
	// MaxFrameBytes is the largest frame of a compressed mode: the size asked
	// of the node, and once a node has taken the mode, the size its kernel
	// gave. Zero for a picture mode, whose frames are all frameBytes long.
	MaxFrameBytes uint32
}

// asksWhat reports whether a node opened in other would be asked for what m
// asks: the same mode, whatever maximum frame size a kernel gave either.
func (m sharedMode) asksWhat(other sharedMode) bool {
	m.MaxFrameBytes, other.MaxFrameBytes = min(m.MaxFrameBytes, 1), min(other.MaxFrameBytes, 1)
	return m == other
}

// String reads "1280x720 YUYV, 30 fps"; the fourcc is trimmed ("Y16 " -> "Y16").
func (m sharedMode) String() string {
	return fmt.Sprintf("%dx%d %s, %d fps", m.Width, m.Height, strings.TrimSpace(m.Fourcc), m.FPS)
}

func describeShareNode(mode sharedMode, stamp string) string {
	return fmt.Sprintf("%s, %s", mode, describeShareSettings(mode.Format, stamp))
}

func describeShareSettings(format, stamp string) string {
	return fmt.Sprintf("format %s, stamp %s", format, stamp)
}

func (m sharedMode) frameBytes() int { return int(m.BytesPerLine) * int(m.Height) }

func (m sharedMode) compressed() bool { return m.MaxFrameBytes > 0 }

func (m sharedMode) bufferBytes() int {
	if m.compressed() {
		return int(m.MaxFrameBytes)
	}
	return m.frameBytes()
}

type compressedNodeFormat struct {
	v4l2 uint32
	name string
}

var compressedNodeFormats = map[agentpb.VideoCodec]compressedNodeFormat{
	videoCodecMJPEG:                     {v4l2PixFmtMJPEG, "JPEG"},
	agentpb.VideoCodec_VIDEO_CODEC_H264: {v4l2PixFmtH264, "H.264"},
}

// pixelFormat is the V4L2 pixel format of the mode's fourcc, or 0 for a fourcc
// no node can declare.
func (m sharedMode) pixelFormat() uint32 {
	if m.compressed() {
		for _, f := range compressedNodeFormats {
			if fourccName(f.v4l2) == m.Fourcc {
				return f.v4l2
			}
		}
		return 0
	}
	for _, f := range rawPixelFormats {
		if f.fourcc == m.Fourcc {
			return f.v4l2
		}
	}
	return 0
}

func compressedMode(codec agentpb.VideoCodec, format string, width, height, fps uint32) sharedMode {
	return sharedMode{
		Width: width, Height: height, Fourcc: fourccName(compressedNodeFormats[codec].v4l2), FPS: fps,
		Format: format, MaxFrameBytes: maxFrameBytes,
	}
}

// carries reports whether f is a frame a node in this mode declares: a picture
// in exactly its layout, or a compressed frame of its codec.
func (m sharedMode) carries(f *videoFrame) bool {
	if rf := f.rawFmt; rf != nil {
		return rf.GetWidth() == m.Width && rf.GetHeight() == m.Height &&
			strings.TrimSpace(rf.GetFourcc()) == strings.TrimSpace(m.Fourcc) &&
			rf.GetBytesPerLine() == m.BytesPerLine
	}
	declared, known := compressedNodeFormats[f.codec]
	return m.compressed() && known && fourccName(declared.v4l2) == m.Fourcc
}

func describeNodeFrame(f *videoFrame) string {
	if f.rawFmt != nil {
		return describeRawFormat(f.rawFmt)
	}
	if declared, known := compressedNodeFormats[f.codec]; known {
		return declared.name
	}
	return strings.TrimPrefix(f.codec.String(), "VIDEO_CODEC_")
}

// admits reports whether a stream request is satisfied by this mode: every
// width, height and framerate the request names equals the mode's.
func (m sharedMode) admits(req *agentpb.StreamVideoRequest) bool {
	if w := req.GetWidth(); w != 0 && w != m.Width {
		return false
	}
	if h := req.GetHeight(); h != 0 && h != m.Height {
		return false
	}
	if f := req.GetFramerate(); f != 0 && f != m.FPS {
		return false
	}
	return true
}

// shareDecision says whether a camera is shared and, when it is, at what mode;
// otherwise Reason says why not. AskedFormat is set when the camera is shared
// as pictures because it cannot be declared in the format the app asked for,
// and AskedWhyNot says why it cannot.
type shareDecision struct {
	Shared                   bool
	Mode                     sharedMode
	Reason                   string
	AskedFormat, AskedWhyNot string
}

func describeServedInstead(askedFormat, servedFormat, whyNot string) string {
	return fmt.Sprintf("asked for %s; serving %s because %s", askedFormat, servedFormat, whyNot)
}

// defaultSharedFPS is the rate declared when the camera advertises none.
const defaultSharedFPS = 30

// isDepthOrInfraredNode is behind a var so tests can describe a RealSense node
// without sysfs.
var isDepthOrInfraredNode = isRealSenseDepthNode

// shareDecisionFor decides whether the camera dev is served to an app that
// asks for format through an agent-fed node, and at which mode. An app gets a
// node for every camera that can have one: a camera that cannot be declared in
// the format asked for is served as pictures when it can be shared at all.
// The first failing rule gives the Reason.
func (s *VideoService) shareDecisionFor(_ context.Context, dev *agentpb.VideoDevice, format string) shareDecision {
	pictures := appconfig.CameraFormats[0]
	if format == pictures {
		return s.shareDecisionsFor(dev, format)[format]
	}
	decisions := s.shareDecisionsFor(dev, format, pictures)
	asked, instead := decisions[format], decisions[pictures]
	if asked.Shared {
		return asked
	}
	if instead.Shared {
		instead.AskedFormat, instead.AskedWhyNot = format, asked.Reason
	}
	return instead
}

// shareDecisionsFor decides each format on its own, with no pictures served
// instead; the rules that do not depend on the format are checked once.
func (s *VideoService) shareDecisionsFor(dev *agentpb.VideoDevice, formats ...string) map[string]shareDecision {
	decisions := make(map[string]shareDecision, len(formats))
	gstPath, available, reason := s.shareRefusalFor(dev)
	for _, format := range formats {
		switch {
		case reason != "":
			decisions[format] = shareDecision{Reason: reason}
		case format == appconfig.CameraFormatPixels:
			decisions[format] = pictureShareDecision(dev, gstPath, available)
		default:
			decisions[format] = s.compressedShareDecision(dev, format)
		}
	}
	return decisions
}

// shareRefusalFor is why the camera dev is served to no app through an
// agent-fed node, whatever its format, or "" with the GStreamer listing.
func (s *VideoService) shareRefusalFor(dev *agentpb.VideoDevice) (gstPath string, available map[string]bool, reason string) {
	refuse := func(reason string) (string, map[string]bool, string) { return "", nil, reason }

	if s.loopback == nil || s.loopback.Available() != nil {
		return refuse("no compatible v4l2loopback module on this device")
	}
	switch dev.GetTransport() {
	case agentpb.VideoTransport_VIDEO_TRANSPORT_USB:
	case agentpb.VideoTransport_VIDEO_TRANSPORT_CSI:
		return refuse("CSI cameras are not shared yet")
	default:
		return refuse("only USB cameras are shared in this release")
	}
	if isDepthOrInfraredNode(dev.GetPath()) {
		return refuse("depth and infrared nodes are not shared")
	}
	if dev.GetThermal() != nil {
		return refuse("thermal cameras are not shared; an app reads their measurements at a mode it picks")
	}

	var formats []uint32
	if s.capturePixelFormats != nil {
		if got, known := s.capturePixelFormats(dev.GetPath()); known {
			formats = got
		}
	}
	for _, f := range formats {
		if f == v4l2PixFmtH264 {
			return refuse("the camera produces H.264 itself; sharing it comes in a later release")
		}
	}
	if len(formats) == 0 {
		return refuse("the camera's formats could not be read")
	}

	gstPath, available, err := s.gstElements()
	if err != nil {
		return refuse("GStreamer is not installed on this device")
	}
	return gstPath, available, ""
}

func pictureShareDecision(dev *agentpb.VideoDevice, gstPath string, available map[string]bool) shareDecision {
	refuse := func(reason string) shareDecision { return shareDecision{Reason: reason} }

	plan, err := planGStreamerPipelineShaped(pipelineShape{encode: false}, gstPath, dev.GetPath(),
		&agentpb.StreamVideoRequest{}, "", false, camera.TransportUSB, "", pipeWireSource{}, available)
	if err != nil && !errors.Is(err, errNoTapOnlyPipeline) {
		return refuse("no shared mode: " + err.Error())
	}
	if plan.pixels == nil {
		return refuse("no shared mode: " + plan.rawReason())
	}

	mode := sharedMode{
		Width:        plan.pixels.GetWidth(),
		Height:       plan.pixels.GetHeight(),
		Fourcc:       plan.pixels.GetFourcc(),
		BytesPerLine: plan.pixels.GetBytesPerLine(),
		FPS:          defaultSharedFPS,
		Format:       appconfig.CameraFormatPixels,
	}
	captureFormat := uint32(v4l2PixFmtMJPEG)
	if plan.raw != nil {
		captureFormat = mode.pixelFormat()
	}
	if best := highestFrameRateFor(dev.GetPath(), captureFormat, mode.Width, mode.Height); best > 0 {
		mode.FPS = best
	}
	return shareDecision{Shared: true, Mode: mode}
}

// shareFormatCodecs is the codec of the frames a compressed node format
// carries, for the formats that fix one; a format absent here carries the
// codec of the camera's own frames.
var shareFormatCodecs = map[string]agentpb.VideoCodec{
	appconfig.CameraFormatH264: agentpb.VideoCodec_VIDEO_CODEC_H264,
}

// ownFramesAtDefaults is the codec, size and rate of the camera's own frames
// as a capture that names no parameters hands them on whole, or why it hands
// on none.
func (s *VideoService) ownFramesAtDefaults(path string) (codec agentpb.VideoCodec, width, height, fps uint32, declined string) {
	width, height, fps, declined = s.wholeJPEGCapture(path, &agentpb.StreamVideoRequest{})
	if declined != "" {
		return 0, 0, 0, 0, declined
	}
	return videoCodecMJPEG, width, height, fps, ""
}

const ownFramesNotWhole = "the camera's own frames are not captured whole: %s"

// compressedShareDecision declares a compressed node at the size and rate of
// the camera's own whole frames: the H.264 made from them has that size too.
func (s *VideoService) compressedShareDecision(dev *agentpb.VideoDevice, format string) shareDecision {
	codec, width, height, fps, declined := s.ownFramesAtDefaults(dev.GetPath())
	if declined != "" {
		return shareDecision{Reason: fmt.Sprintf(ownFramesNotWhole, declined)}
	}
	if fixed, ok := shareFormatCodecs[format]; ok {
		codec = fixed
	}
	if fps == 0 {
		fps = defaultSharedFPS
	}
	return shareDecision{Shared: true, Mode: compressedMode(codec, format, width, height, fps)}
}

// nodeNeedsOwnFrames reports whether a node gets frames only while the
// camera's own frames are captured whole: it carries those frames or frames
// made from them, or its stamp is the frame number, which a picture has only
// when it is made from one of them.
func nodeNeedsOwnFrames(mode sharedMode, stamp string) bool {
	return mode.compressed() || stamp == appconfig.CameraStampFrame
}

const picturesWithoutFrameNumbers = `the camera's own frames are not captured whole (%s), so its pictures carry no frame number for the "frame" stamp`

// nodeUnservedAt answers for a capture that names no parameters, which is how
// a share runs the camera. It captures nothing.
func (s *VideoService) nodeUnservedAt(path string, mode sharedMode, stamp string) string {
	if !nodeNeedsOwnFrames(mode, stamp) {
		return ""
	}
	_, _, _, _, declined := s.ownFramesAtDefaults(path)
	switch {
	case declined == "":
		return ""
	case mode.compressed():
		return fmt.Sprintf(ownFramesNotWhole, declined)
	default:
		return fmt.Sprintf(picturesWithoutFrameNumbers, declined)
	}
}

// gstElementsMemo is a successful gst listing kept for the service's life, and
// the one listing in flight, if any.
type gstElementsMemo struct {
	mu        sync.Mutex
	gstPath   string
	available map[string]bool
	inflight  *gstListing
}

// gstListing is one attempt at listing; callers that arrive meanwhile wait on done.
type gstListing struct {
	done      chan struct{}
	gstPath   string
	available map[string]bool
	err       error
}

// gstDiscover finds gst-launch-1.0 and lists the installed elements. Behind a
// var so tests can count and fail listings.
var gstDiscover = func() (string, map[string]bool, error) {
	launch, err := resolveGSTBinary("gst-launch-1.0")
	if err != nil {
		return "", nil, err
	}
	inspect, err := resolveGSTBinary("gst-inspect-1.0")
	if err != nil {
		return "", nil, err
	}
	elements, err := listGSTElements(inspect)
	if err != nil {
		return "", nil, err
	}
	return launch, elements, nil
}

// gstElements finds gst-launch-1.0 and lists the installed elements, once. The
// returned element map is shared and read-only. Callers that arrive while a
// listing runs wait for its result instead of starting another; a failure is
// shared with those waiting but not remembered, so installing GStreamer later
// works without a restart. The lock is never held across the listing.
func (s *VideoService) gstElements() (gstPath string, available map[string]bool, err error) {
	m := &s.gstMemo
	m.mu.Lock()
	if m.available != nil {
		defer m.mu.Unlock()
		return m.gstPath, m.available, nil
	}
	if l := m.inflight; l != nil {
		m.mu.Unlock()
		<-l.done
		return l.gstPath, l.available, l.err
	}
	l := &gstListing{done: make(chan struct{})}
	m.inflight = l
	m.mu.Unlock()

	l.gstPath, l.available, l.err = gstDiscover()

	m.mu.Lock()
	if l.err == nil {
		m.gstPath, m.available = l.gstPath, l.available
	}
	m.inflight = nil
	m.mu.Unlock()
	close(l.done)
	return l.gstPath, l.available, l.err
}

// v4l2FrmIvalEnum matches struct v4l2_frmivalenum (52 bytes). The union is the
// larger stepwise variant; only the discrete numerator/denominator are read.
type v4l2FrmIvalEnum struct {
	Index       uint32
	PixelFormat uint32
	Width       uint32
	Height      uint32
	Type        uint32
	Numerator   uint32 // discrete.numerator
	Denominator uint32 // discrete.denominator
	_           [4]uint32
	_           [2]uint32
}

var (
	_ [52 - unsafe.Sizeof(v4l2FrmIvalEnum{})]byte
	_ [unsafe.Sizeof(v4l2FrmIvalEnum{}) - 52]byte
	_ [20 - unsafe.Offsetof(v4l2FrmIvalEnum{}.Numerator)]byte
	_ [unsafe.Offsetof(v4l2FrmIvalEnum{}.Numerator) - 20]byte
)

// A shared camera's mode declares this rate and a capture that was asked for
// none runs at it, so the two agree. Zero when the camera advertises no rate.
func highestFrameRateFor(path string, pixfmt, width, height uint32) uint32 {
	var best uint32
	for _, rate := range frameRatesFor(path, pixfmt, width, height) {
		best = max(best, rate)
	}
	return best
}

// vidiocEnumFrameintervals is VIDIOC_ENUM_FRAMEINTERVALS.
const vidiocEnumFrameintervals = 0xC034564B

// v4l2FrmivalTypeDiscrete is V4L2_FRMIVAL_TYPE_DISCRETE.
const v4l2FrmivalTypeDiscrete = 1

// frameRatesFor lists the discrete frame rates (frames per second, rounded) a
// camera advertises for one format and size. Read-only open, so it answers
// while the camera is streaming. Behind a var so tests can describe a camera.
var frameRatesFor = func(path string, pixfmt, width, height uint32) []uint32 {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	defer unix.Close(fd) //nolint:errcheck
	var rates []uint32
	for index := uint32(0); index < 64; index++ {
		e := v4l2FrmIvalEnum{Index: index, PixelFormat: pixfmt, Width: width, Height: height}
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocEnumFrameintervals,
			uintptr(unsafe.Pointer(&e))); errno != 0 {
			break
		}
		if e.Type != v4l2FrmivalTypeDiscrete || e.Numerator == 0 {
			continue
		}
		rates = append(rates, uint32(math.Round(float64(e.Denominator)/float64(e.Numerator))))
	}
	return rates
}
