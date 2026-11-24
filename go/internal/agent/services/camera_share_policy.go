package services

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"

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

// formatCarriesPictures reports whether a node of this entitlement format
// carries decoded pictures, each exactly one frame size long.
func formatCarriesPictures(format string) bool { return format == appconfig.CameraFormatPixels }

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
// otherwise Reason says why not. AskedFormat is set when the node serves
// another format than the app asked for, because the camera cannot be declared
// in that one, and AskedWhyNot says why it cannot. SoftwareEncoder names the
// software element that makes the node's frames on this device, and is empty
// for a node that carries what the camera produces, for pictures, and for an
// encoder not known to be software.
type shareDecision struct {
	Shared                   bool
	Mode                     sharedMode
	Reason                   string
	AskedFormat, AskedWhyNot string
	SoftwareEncoder          string
}

func describeSoftwareEncoder(element string) string {
	return "fed by the software encoder " + element
}

func describeServedInstead(askedFormat, servedFormat, whyNot string) string {
	return fmt.Sprintf("asked for %s; serving %s because %s", askedFormat, servedFormat, whyNot)
}

// defaultSharedFPS is the rate declared when the camera advertises none.
const defaultSharedFPS = 30

// shareDecisionFor decides whether the camera dev is served to an app that
// asks for format through an agent-fed node, and at which mode. An app gets a
// node for every camera that can have one: a camera that cannot be declared in
// the format asked for is served in the first format it can be declared in
// (appconfig.CameraFormatsServedFor). A camera with no mode at all gives the
// reason it has no pictures.
func (s *VideoService) shareDecisionFor(_ context.Context, dev *agentpb.VideoDevice, format string) shareDecision {
	candidates := appconfig.CameraFormatsServedFor(format)
	decisions := s.shareDecisionsFor(dev, candidates...)
	for _, served := range candidates {
		decision := decisions[served]
		if !decision.Shared {
			continue
		}
		if served != format {
			decision.AskedFormat, decision.AskedWhyNot = format, decisions[format].Reason
		}
		return decision
	}
	return decisions[appconfig.CameraFormatPixels]
}

// shareDecisionsFor decides each format on its own, with no other format
// served instead. What the camera has for a node is the capture's to say
// (framesForNodes); this only declares a mode for it.
func (s *VideoService) shareDecisionsFor(dev *agentpb.VideoDevice, formats ...string) map[string]shareDecision {
	decisions := make(map[string]shareDecision, len(formats))
	if s.loopback == nil || s.loopback.Available() != nil {
		for _, format := range formats {
			decisions[format] = shareDecision{Reason: "no compatible v4l2loopback module on this device"}
		}
		return decisions
	}
	frames := s.framesForNodes(dev)
	for _, format := range formats {
		decisions[format] = shareDecisionOf(frames, format)
	}
	return decisions
}

func shareDecisionOf(frames nodeFrames, format string) shareDecision {
	if whyNot := frames.whyNo(format); whyNot != "" {
		return shareDecision{Reason: whyNot}
	}
	var mode sharedMode
	if formatCarriesPictures(format) {
		mode = sharedMode{
			Width: frames.pictures.GetWidth(), Height: frames.pictures.GetHeight(), Fourcc: frames.pictures.GetFourcc(),
			BytesPerLine: frames.pictures.GetBytesPerLine(), FPS: frames.pictureRate, Format: format,
		}
	} else {
		mode = compressedMode(frames.nodeCodec(format), format, frames.own.width, frames.own.height, frames.own.framerate)
	}
	if mode.FPS == 0 {
		mode.FPS = defaultSharedFPS
	}
	decision := shareDecision{Shared: true, Mode: mode}
	if mode.compressed() && frames.nodeCodec(format) != frames.own.codec {
		decision.SoftwareEncoder = frames.softwareEncoder
	}
	return decision
}

// shareFormatCodecs is the codec of the frames a compressed node format
// carries, for the formats that fix one; a format absent here carries the
// codec of the camera's own frames.
var shareFormatCodecs = map[string]agentpb.VideoCodec{
	appconfig.CameraFormatH264: agentpb.VideoCodec_VIDEO_CODEC_H264,
}

// ownFramesAtDefaults is the camera's own frames as a capture that names no
// parameters hands them on whole, or why it hands on none. native says a
// native source captures the camera either way.
func (s *VideoService) ownFramesAtDefaults(path string) (own ownFrames, declined string, native bool) {
	return s.ownFramesFor(path, &agentpb.StreamVideoRequest{})
}

// Why a node gets no frames from a camera that is captured. Each says what
// the developer can do: a node keeps the mode it was made with, so the way to
// another one is another deploy.
const (
	picturesWithoutFrameNumbers = `the camera's own frames are not captured whole (%s), so its pictures carry no frame number for the "frame" stamp`
	picturesNotMade             = `no pictures are made from this camera's frames (%s); deploy the app again and its node carries the camera's own frames, or set "format": "native" on its camera entitlement`
	nodeModeNotDelivered        = `the camera now delivers %s and the node declares %s; deploy the app again to get a node for what the camera delivers`
)

// nodeUnservedAt is why a default capture of the camera at path, which is how
// a share runs it, has no frames for a node of this mode and stamp, or "". It
// captures nothing: it asks what the capture sources would hand on now, which
// is not what they listed when the node was made once one of them has refused
// the camera or the pictures decoded from it have been given up.
func (s *VideoService) nodeUnservedAt(path string, mode sharedMode, stamp string) string {
	own, declined, native := s.ownFramesAtDefaults(path)
	switch {
	case declined != "" && mode.compressed():
		return fmt.Sprintf(ownFramesNotWhole, declined)
	case declined != "" && stamp == appconfig.CameraStampFrame:
		return fmt.Sprintf(picturesWithoutFrameNumbers, declined)
	case declined != "" || !native:
		return ""
	case mode.compressed():
		delivered := nodeFrames{own: own}.nodeCodec(mode.Format)
		if fourccName(compressedNodeFormats[delivered].v4l2) != mode.Fourcc {
			return fmt.Sprintf(nodeModeNotDelivered, compressedNodeFormats[delivered].name, mode)
		}
		return ""
	}
	pictures, whyNot := s.decodedPictures(path, own)
	switch {
	case pictures == nil:
		return fmt.Sprintf(picturesNotMade, whyNot)
	case !mode.carries(&videoFrame{rawFmt: pictures}):
		return fmt.Sprintf(nodeModeNotDelivered, describeRawFormat(pictures), mode)
	}
	return ""
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
