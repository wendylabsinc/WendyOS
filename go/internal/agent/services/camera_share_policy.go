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
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// sharedMode is the one picture format an app's agent-fed camera node declares.
// It always comes from the planner (see shareDecisionFor), so the node declares
// exactly what the hub's pixel tap produces.
type sharedMode struct {
	Width, Height uint32
	Fourcc        string
	BytesPerLine  uint32
	FPS           uint32
}

// String reads "1280x720 YUYV, 30 fps"; the fourcc is trimmed ("Y16 " -> "Y16").
func (m sharedMode) String() string {
	return fmt.Sprintf("%dx%d %s, %d fps", m.Width, m.Height, strings.TrimSpace(m.Fourcc), m.FPS)
}

// frameBytes is the size of one frame in this mode.
func (m sharedMode) frameBytes() int { return int(m.BytesPerLine) * int(m.Height) }

// pixelFormat is the V4L2 pixel format of the mode's fourcc, or 0 for a fourcc
// the raw table does not know.
func (m sharedMode) pixelFormat() uint32 {
	for _, f := range rawPixelFormats {
		if f.fourcc == m.Fourcc {
			return f.v4l2
		}
	}
	return 0
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
// otherwise Reason says why not.
type shareDecision struct {
	Shared bool
	Mode   sharedMode
	Reason string
}

// defaultSharedFPS is the rate declared when the camera advertises none.
const defaultSharedFPS = 30

// isDepthOrInfraredNode is behind a var so tests can describe a RealSense node
// without sysfs.
var isDepthOrInfraredNode = isRealSenseDepthNode

// shareDecisionFor decides whether the camera dev is served to apps through an
// agent-fed node, and at which mode. The first failing rule gives the Reason.
func (s *VideoService) shareDecisionFor(ctx context.Context, dev *agentpb.VideoDevice) shareDecision {
	refuse := func(reason string) shareDecision { return shareDecision{Reason: reason} }

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
	}
	captureFormat := uint32(v4l2PixFmtMJPEG)
	if plan.raw != nil {
		captureFormat = mode.pixelFormat()
	}
	var best uint32
	for _, r := range frameRatesFor(dev.GetPath(), captureFormat, mode.Width, mode.Height) {
		if r > best {
			best = r
		}
	}
	if best > 0 {
		mode.FPS = best
	}
	return shareDecision{Shared: true, Mode: mode}
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
