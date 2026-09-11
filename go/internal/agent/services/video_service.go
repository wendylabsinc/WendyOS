package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/audio"
	"github.com/wendylabsinc/wendy/go/internal/agent/board"
	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// V4L2 ioctl constants for Linux kernel video capture interface.
const (
	v4l2BufTypeVideoCapture = 1
	v4l2MemoryMmap          = 1
	v4l2PixFmtH264          = 0x34363248 // 'H264' little-endian FourCC
	v4l2PixFmtYUYV          = 0x56595559 // 'YUYV'
	v4l2PixFmtMJPEG         = 0x47504A4D // 'MJPG'
	v4l2PixFmtUYVY          = 0x59565955 // 'UYVY'
	v4l2PixFmtY16           = 0x20363159 // 'Y16 ' -- note the trailing space
	v4l2PixFmtGrey          = 0x59455247 // 'GREY'
	v4l2FieldNone           = 1

	v4l2CapVideoCapture = 0x00000001
	v4l2CapMetaCapture  = 0x00800000
	v4l2CapDeviceCaps   = 0x80000000

	vidiocQueryCap  = 0x80685600
	vidiocSFmt      = 0xC0D05605
	vidiocReqbufs   = 0xC0145608
	vidiocQuerybuf  = 0xC0585609
	vidiocQbuf      = 0xC058560F
	vidiocDqbuf     = 0xC0585611
	vidiocStreamon  = 0x40045612
	vidiocStreamoff = 0x40045613
	vidiocSExtCtrls = 0xC0205648 // _IOWR('V', 72, struct v4l2_ext_controls), 32 bytes
	// _IOWR('V', 74, struct v4l2_frmsizeenum): 4+4+4 + 24 (stepwise union) + 8 = 44 bytes.
	vidiocEnumFramesizes = 0xC02C564A

	v4l2FrmsizeTypeDiscrete = 1

	// Upper bound when picking a default resolution. The hub re-encodes and fans
	// out to every subscriber, so an unbounded "pick the biggest" would turn a
	// 4K webcam into a CPU and bandwidth problem for viewers that never asked
	// for it.
	//
	// 720p, not 1080p: both codec halves of the default path can land on a CPU,
	// and 1080p is over the line on real fleet hardware. Encode: a camera
	// without onboard H.264 falls back to x264enc, measured at ~5 fps for
	// 1080p on a Jetson AGX-class host vs full rate at 720p. Decode: the
	// Orin Nano has no NVDEC, and software avdec_h264 measured ~5.7 fps at
	// 1080p on it. Callers that want more than 720p can request it
	// explicitly; this bound only decides what "no preference" means.
	// Raising it conditionally when a hardware encoder was actually
	// selected is a possible follow-up.
	defaultMaxDefaultWidth  = 1280
	defaultMaxDefaultHeight = 720

	// Encoder control IDs and class. V4L2_CID_CODEC_BASE = V4L2_CTRL_CLASS_CODEC
	// (0x00990000) | 0x900; the keyframe controls are fixed offsets from it.
	v4l2CtrlClassCodec = 0x00990000 // V4L2_CTRL_CLASS_CODEC
	v4l2CIDGOPSize     = 0x009909CB // V4L2_CID_MPEG_VIDEO_GOP_SIZE (base+203)
	v4l2CIDH264IPeriod = 0x00990A66 // V4L2_CID_MPEG_VIDEO_H264_I_PERIOD (base+358)
)

// v4l2Format matches struct v4l2_format (208 bytes) for V4L2_BUF_TYPE_VIDEO_CAPTURE.
type v4l2Format struct {
	Type         uint32
	_            [4]byte // align the v4l2_format union to 8 bytes on 64-bit Linux
	Width        uint32
	Height       uint32
	PixelFormat  uint32
	Field        uint32
	BytesPerLine uint32
	SizeImage    uint32
	Colorspace   uint32
	Priv         uint32
	Flags        uint32
	Enc          uint32
	Quantization uint32
	XferFunc     uint32
	_            [152]byte
}

// VIDIOC_S_FMT encodes a 208-byte argument, and the anonymous format union
// starts at byte 8 on Linux's 64-bit UAPI. Keep both properties compile-time
// checked because shifted fields make valid H.264 devices look unsupported.
var (
	_ [208 - unsafe.Sizeof(v4l2Format{})]byte
	_ [unsafe.Sizeof(v4l2Format{}) - 208]byte
	_ [8 - unsafe.Offsetof(v4l2Format{}.Width)]byte
	_ [unsafe.Offsetof(v4l2Format{}.Width) - 8]byte
)

// v4l2FrmSizeEnum matches struct v4l2_frmsizeenum (44 bytes). Only the discrete
// branch of the union is read; Union covers the larger stepwise variant so the
// struct size matches the ioctl's expectation.
type v4l2FrmSizeEnum struct {
	Index       uint32
	PixelFormat uint32
	Type        uint32
	Width       uint32 // discrete.width
	Height      uint32 // discrete.height
	_           [16]byte
	_           [8]byte
}

// bestDefaultFrameSize returns the largest discrete frame size the device
// advertises for pixfmt, capped at 720p, or (0,0) if enumeration yields
// nothing usable (stepwise-only devices, or drivers without the ioctl).
//
// Why this exists: VIDIOC_S_FMT with width=height=0 does NOT mean "device
// default". V4L2 requires drivers to adjust invalid values to the nearest
// supported ones, and UVC resolves 0x0 to its SMALLEST frame size — 352x288 on
// a common Arducam module, versus the 640x480+ the same camera offers. A
// caller asking for "no preference" wants the device's best sensible mode, not
// its worst, so enumerate and choose rather than letting the driver clamp.
// hasStandardAspect reports whether w:h is a normal picture aspect ratio.
//
// This is how we avoid picking a mode that carries non-image data. Thermal
// modules in the TOPDON TC001 / InfiRay family advertise their sensor size AND
// a taller variant with metadata rows stacked on top of the picture — the same
// image plus ~100 rows of raw temperature. That variant has the LARGER area, so
// a pure largest-area rule picks it and the caller gets a band of false-colour
// noise across the top of every frame. Observed on wendy-box-theta and ccr1:
// 512x484 was selected over 512x384, and the extra 100 rows rendered as a green
// stripe in both the CLI viewer and the companion app.
//
// Real picture modes are 4:3, 16:9, 16:10, 3:2 or 5:4; the stacked variants land
// on none of them (512x484 is 1.058). Tolerance is loose enough for sizes that
// do not divide exactly (e.g. 644x384 = 1.677 is not 16:9 and is correctly
// excluded, while 320x240 and 1280x720 match exactly).
func hasStandardAspect(w, h uint32) bool {
	if w == 0 || h == 0 {
		return false
	}
	ratio := float64(w) / float64(h)
	for _, std := range []float64{4.0 / 3.0, 16.0 / 9.0, 16.0 / 10.0, 3.0 / 2.0, 5.0 / 4.0} {
		if math.Abs(ratio-std) <= 0.02 {
			return true
		}
	}
	return false
}

func bestDefaultFrameSize(fd int, pixfmt uint32) (uint32, uint32) {
	// Tracked separately so a standard-aspect mode always wins, even when a
	// non-standard one is larger. Falling back to the largest-area mode keeps
	// behaviour unchanged for cameras that advertise nothing standard.
	var bestW, bestH uint32         // best standard-aspect mode
	var fallbackW, fallbackH uint32 // best of any aspect
	for index := uint32(0); index < 64; index++ {
		fse := v4l2FrmSizeEnum{Index: index, PixelFormat: pixfmt}
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocEnumFramesizes,
			uintptr(unsafe.Pointer(&fse))); errno != 0 {
			break // EINVAL marks the end of the list; anything else is unusable too
		}
		if fse.Type != v4l2FrmsizeTypeDiscrete {
			break // stepwise/continuous: no discrete list to choose from
		}
		if fse.Width > defaultMaxDefaultWidth || fse.Height > defaultMaxDefaultHeight {
			continue
		}
		area := uint64(fse.Width) * uint64(fse.Height)
		if area > uint64(fallbackW)*uint64(fallbackH) {
			fallbackW, fallbackH = fse.Width, fse.Height
		}
		if hasStandardAspect(fse.Width, fse.Height) &&
			area > uint64(bestW)*uint64(bestH) {
			bestW, bestH = fse.Width, fse.Height
		}
	}
	if bestW != 0 {
		return bestW, bestH
	}
	return fallbackW, fallbackH
}

// bestDefaultFrameSizeForDevice opens path just long enough to ask what the
// camera can do, and returns the largest discrete size across the pixel formats
// the GStreamer path can negotiate. (0,0) when the device cannot be opened or
// advertises nothing discrete, in which case the caller leaves caps unset and
// gets the old behaviour.
//
// This exists because most USB webcams have no onboard H.264, so
// streamV4L2Native returns nativeH264NotSupported and the GStreamer path runs
// instead — where omitting width/height from the caps lets the source negotiate
// its first (smallest) mode, the same 352x288 trap the native path had.
// Behind a var, like deviceSupportsMJPEGSize, so pipeline-construction tests
// can describe a camera without a V4L2 node.
var bestDefaultFrameSizeForDevice = func(path string) (uint32, uint32) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return 0, 0
	}
	defer unix.Close(fd) //nolint:errcheck

	var bestW, bestH uint32
	for _, pixfmt := range []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG} {
		w, h := bestDefaultFrameSize(fd, pixfmt)
		if uint64(w)*uint64(h) > uint64(bestW)*uint64(bestH) {
			bestW, bestH = w, h
		}
	}
	return bestW, bestH
}

// deviceSupportsMJPEGSize reports whether the device advertises MJPEG output at
// exactly width x height. Behind a var so pipeline-construction tests can fake
// camera capabilities without a real V4L2 device (same spirit as gstFallbackDirs).
var deviceSupportsMJPEGSize = func(path string, width, height uint32) bool {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd) //nolint:errcheck
	for index := uint32(0); index < 64; index++ {
		fse := v4l2FrmSizeEnum{Index: index, PixelFormat: v4l2PixFmtMJPEG}
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocEnumFramesizes,
			uintptr(unsafe.Pointer(&fse))); errno != 0 {
			return false
		}
		if fse.Type != v4l2FrmsizeTypeDiscrete {
			return false
		}
		if fse.Width == width && fse.Height == height {
			return true
		}
	}
	return false
}

// v4l2ReqBuffers matches struct v4l2_requestbuffers (20 bytes).
type v4l2ReqBuffers struct {
	Count        uint32
	Type         uint32
	Memory       uint32
	Capabilities uint32
	Flags        uint32
}

// v4l2Buf is a fixed-size byte array matching struct v4l2_buffer (88 bytes on 64-bit Linux).
// Accessor methods read/write fields at their known offsets to avoid C-struct alignment surprises.
type v4l2Buf [88]byte

func (b *v4l2Buf) index() uint32     { return *(*uint32)(unsafe.Pointer(&b[0])) }
func (b *v4l2Buf) setIndex(i uint32) { *(*uint32)(unsafe.Pointer(&b[0])) = i }
func (b *v4l2Buf) setType(t uint32)  { *(*uint32)(unsafe.Pointer(&b[4])) = t }
func (b *v4l2Buf) bytesUsed() uint32 { return *(*uint32)(unsafe.Pointer(&b[8])) }
func (b *v4l2Buf) flags() uint32     { return *(*uint32)(unsafe.Pointer(&b[12])) }
func (b *v4l2Buf) setFlags(f uint32) { *(*uint32)(unsafe.Pointer(&b[12])) = f }
func (b *v4l2Buf) timestampNanos() int64 {
	sec := *(*int64)(unsafe.Pointer(&b[24]))
	usec := *(*int64)(unsafe.Pointer(&b[32]))
	return sec*int64(time.Second) + usec*int64(time.Microsecond)
}

// setTimestampNanos writes nanos into the struct timeval at offsets 24 and 32,
// mirroring timestampNanos above exactly so a value written here reads back
// through that accessor.
//
// The microsecond field truncates: struct timeval has no finer resolution. The
// truncation is exact rather than lossy-and-rounded, because 1e9 is divisible
// by 1000, so sec*1e6 + usec == nanos/1000 for any non-negative nanos. That
// identity is what lets a consumer derive the expected buffer value from
// FrameIdentity.boottime_nanos without a second field on the wire.
func (b *v4l2Buf) setTimestampNanos(nanos int64) {
	*(*int64)(unsafe.Pointer(&b[24])) = nanos / int64(time.Second)
	*(*int64)(unsafe.Pointer(&b[32])) = (nanos % int64(time.Second)) / int64(time.Microsecond)
}
func (b *v4l2Buf) sequence() uint32   { return *(*uint32)(unsafe.Pointer(&b[56])) }
func (b *v4l2Buf) setMemory(m uint32) { *(*uint32)(unsafe.Pointer(&b[60])) = m }
func (b *v4l2Buf) offset() uint32     { return *(*uint32)(unsafe.Pointer(&b[64])) }

// v4l2Capability matches struct v4l2_capability (104 bytes).
type v4l2Capability struct {
	Driver       [16]byte
	Card         [32]byte
	BusInfo      [32]byte
	Version      uint32
	Capabilities uint32
	DeviceCaps   uint32
	Reserved     [3]uint32
}

func (c *v4l2Capability) hasVideoCapture() bool {
	caps := c.Capabilities
	if caps&v4l2CapDeviceCaps != 0 {
		caps = c.DeviceCaps
	}
	// A usable capture node must advertise VIDEO_CAPTURE. Metadata-only companion
	// nodes (e.g. the UVC metadata device some drivers expose on /dev/video1)
	// advertise METADATA_CAPTURE *without* VIDEO_CAPTURE, so the VIDEO_CAPTURE
	// check alone already excludes them. We must NOT additionally exclude on
	// METADATA_CAPTURE: the Raspberry Pi CSI capture node (rp1-cfe) sets both
	// VIDEO_CAPTURE and METADATA_CAPTURE on the same node (device caps
	// 0x24a00001), and excluding it would hide the ribbon camera from
	// `device camera list`.
	return caps&v4l2CapVideoCapture != 0
}

// v4l2ExtControl is a fixed-size array matching the __packed struct
// v4l2_ext_control (20 bytes): id@0, size@4, reserved2@8, then an 8-byte union
// whose __s32 value member sits at offset 12.
type v4l2ExtControl [20]byte

func (c *v4l2ExtControl) setID(id uint32)  { *(*uint32)(unsafe.Pointer(&c[0])) = id }
func (c *v4l2ExtControl) setValue(v int32) { *(*int32)(unsafe.Pointer(&c[12])) = v }
func (c *v4l2ExtControl) value() int32     { return *(*int32)(unsafe.Pointer(&c[12])) }

// v4l2ExtControls is a fixed-size array matching struct v4l2_ext_controls
// (32 bytes): which@0, count@4, error_idx@8, request_fd@12, reserved@16, and a
// pointer to the v4l2_ext_control array@24.
type v4l2ExtControls [32]byte

func (c *v4l2ExtControls) setWhich(w uint32)        { *(*uint32)(unsafe.Pointer(&c[0])) = w }
func (c *v4l2ExtControls) setCount(n uint32)        { *(*uint32)(unsafe.Pointer(&c[4])) = n }
func (c *v4l2ExtControls) setControlsPtr(p uintptr) { *(*uintptr)(unsafe.Pointer(&c[24])) = p }

// nativeH264NotSupported is returned when the V4L2 device does not expose H.264 output.
type nativeH264NotSupported struct{ msg string }

func (e nativeH264NotSupported) Error() string { return e.msg }

// videoFrame carries a single encoded video frame from a producer to subscribers.
// IMMUTABLE after creation: the data slice is allocated once (copied from the V4L2
// mmap region or GStreamer pipe) and never written again. Frames are distributed as
// *videoFrame pointers so all subscribers share the same allocation with zero copies
// at broadcast time. stream.Send() serialises the proto synchronously before returning,
// so reading frame.data without a per-subscriber copy is safe.
type videoFrame struct {
	data          []byte
	tsNs          uint64
	codec         agentpb.VideoCodec
	nativeNs      int64
	nativeClock   string
	nativeFlags   uint32
	sequence      uint32
	sequenceValid bool
	// auAligned reports that data holds exactly one whole encoded access unit.
	// Only the native V4L2 path delivers this (one V4L2 buffer per encoded
	// frame); the GStreamer and IP camera producers read a byte stream from a
	// pipe, so their frames are arbitrary chunk-sized slices of the stream that
	// can begin or end mid-access-unit. Consumers that cut the stream at frame
	// boundaries (snapshot stills, GOP-granularity rate capping) must require
	// this flag: a chunk that merely contains an SPS/PPS/IDR prefix may still
	// be truncated mid-IDR.
	auAligned bool
	// sampleID is the harness-wide identity of this sample within its source,
	// assigned by the hub in produce, once per physical frame arriving from the
	// producer and never again for a further plane, so that EVERY consumer of
	// the frame — an app reading the agent-fed node, the episode capture
	// adapter, a dashboard viewer — names it identically. It is what makes an episode able
	// to say "this is the frame the model saw". The counter is owned by the
	// VideoService per device key, not by the hub, so it does not restart when
	// a producer is torn down and restarted mid-episode.
	sampleID uint64
	// receiptBootNanos is the agent's bracketed CLOCK_BOOTTIME receipt of this
	// frame, taken once in produce so all consumers agree; the bracket
	// half-width is receiptUncertaintyNanos. Zero when the clock read failed.
	receiptBootNanos        int64
	receiptUncertaintyNanos int64
	// rawFmt describes a VIDEO_CODEC_RAW frame's layout; nil for encoded video.
	rawFmt *agentpb.RawFormat
}

type frameTimestamp struct {
	wallNs        uint64
	nativeNs      int64
	nativeClock   string
	nativeFlags   uint32
	sequence      uint32
	sequenceValid bool
	auAligned     bool
}

func realtimeFrameTimestamp(now time.Time) frameTimestamp {
	return frameTimestamp{wallNs: uint64(now.UnixNano()), nativeNs: now.UnixNano(), nativeClock: "CLOCK_REALTIME_AGENT_CAPTURE"}
}

// hubSubscriber is one gRPC stream attached to a hub. Encoded and raw subscribers
// share the producer but receive different frames (see broadcast), and a raw
// subscriber can be turned away by the producer alone — hence its own error.
type hubSubscriber struct {
	ch  chan *videoFrame
	raw bool // wants VIDEO_CODEC_RAW frames instead of encoded video
	// closed and err are set together, under h.mu, when the producer closes this
	// subscriber early (raw was asked for and is not offered). closed keeps
	// broadcast and the final teardown from touching the channel twice.
	closed bool
	err    error
}

// deviceHub multiplexes one camera producer to multiple gRPC subscribers.
type deviceHub struct {
	mu       sync.Mutex
	subs     map[int]*hubSubscriber
	subDrops map[int]uint64
	nextID   int
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{} // closed by runProducer after the device fd is released
	// err is set by runProducer to the terminal error before closing subscriber
	// channels. Nil on graceful shutdown (context cancelled). Protected by h.mu.
	err error
	// width, height, framerate are copied from the request that started this hub.
	// Storing scalars (not a proto pointer) prevents data races if the caller's
	// proto message is ever mutated by middleware after the hub is created.
	width, height, framerate uint32
	// subExplicit records, per subscriber id, the label of the consumer kind
	// that joined with EXPLICITLY asserted stream parameters; subscribers that
	// asserted none (a model app on the sensor socket, the two-plane loopback
	// pump) have no entry. This is the bit the parameter-priority rules pivot
	// on, tracked as hub state at subscribe time rather than inferred later
	// from the parameter values: the hub's parameters count as explicitly held
	// only while at least one such subscriber remains, so a camera whose
	// explicit consumer has left degrades back to a defaulted hub that episode
	// capture may restart. Protected by h.mu.
	subExplicit map[int]string
	// restarted marks that this hub's producer was torn down by an episode
	// capture takeover (takeOverDefaultedHub) rather than by a fault or a
	// normal last-unsubscribe. Parameter-less subscribers use it to reattach
	// to the replacement hub after their channel closes. Protected by h.mu.
	restarted bool
	// sampleSeq is the per-device sample counter, shared with every hub that
	// has served the same device key. It outlives the hub so sample identities
	// stay monotonic across producer restarts within one episode.
	sampleSeq *atomic.Uint64
	// Whether this producer tees raw capture frames (video_raw_tap.go). Undecided
	// until the producer has chosen its capture path; a raw subscriber that joins
	// before then waits, one that joins after a refusal is turned away up front.
	// Protected by h.mu.
	rawState  rawTapState
	rawReason string
	rawFormat *agentpb.RawFormat
}

// maxSubscribersPerHub caps the number of concurrent gRPC streams sharing one
// camera producer. Exceeding this returns ResourceExhausted to the caller.
// The cap bounds per-device channel memory and broadcast work proportionally.
const maxSubscribersPerHub = 16

// subscribe adds a new subscriber and returns its channel and integer ID.
// Returns codes.Unavailable if the hub's context has already been cancelled
// (checked atomically under h.mu so no subscriber can be added to a dying hub),
// or codes.ResourceExhausted if the hub already has maxSubscribersPerHub active subscribers.
func (h *deviceHub) subscribe() (int, chan *videoFrame, error) {
	return h.subscribeAsKind("", false)
}

// subscribeAs is subscribe, additionally recording that this subscriber's join
// asserted the hub's stream parameters explicitly when explicitHolder is
// non-empty. The label names the kind of consumer (see the hubHolder
// constants) so a refused campaign can be told exactly who holds the camera.
func (h *deviceHub) subscribeAs(explicitHolder string) (int, chan *videoFrame, error) {
	return h.subscribeAsKind(explicitHolder, false)
}

// subscribeKind is subscribe with a choice of frames: encoded video (the default)
// or the raw capture frames the producer tees for analytic consumers.
func (h *deviceHub) subscribeKind(raw bool) (int, chan *videoFrame, error) {
	return h.subscribeAsKind("", raw)
}

// subscribeAsKind is the single implementation behind subscribe, subscribeAs and
// subscribeKind. The two dimensions are independent: explicitHolder records who,
// if anyone, asserted the hub's stream parameters, while raw selects which kind
// of frame this subscriber receives.
func (h *deviceHub) subscribeAsKind(explicitHolder string, raw bool) (int, chan *videoFrame, error) {
	ch := make(chan *videoFrame, 4)
	h.mu.Lock()
	if h.ctx.Err() != nil {
		h.mu.Unlock()
		return 0, nil, status.Errorf(codes.Unavailable, "video hub is shutting down")
	}
	if len(h.subs) >= maxSubscribersPerHub {
		h.mu.Unlock()
		return 0, nil, status.Errorf(codes.ResourceExhausted, "too many concurrent streams for this device (max %d)", maxSubscribersPerHub)
	}
	id := h.nextID
	h.nextID++
	h.subs[id] = &hubSubscriber{ch: ch, raw: raw}
	if h.subDrops == nil {
		h.subDrops = make(map[int]uint64)
	}
	h.subDrops[id] = 0
	if explicitHolder != "" {
		if h.subExplicit == nil {
			h.subExplicit = make(map[int]string)
		}
		h.subExplicit[id] = explicitHolder
	}
	h.mu.Unlock()
	return id, ch, nil
}

// subscriberErr returns the error the producer attached when it closed this
// subscriber early, or nil. Read under h.mu for the same reason terminalErr is.
func (h *deviceHub) subscriberErr(id int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sub, ok := h.subs[id]; ok {
		return sub.err
	}
	return nil
}

// unsubscribe removes a subscriber. When the last subscriber leaves it cancels the producer.
// cancel() is called while h.mu is still held to close the race window where a concurrent
// getOrCreateHub could observe h.ctx.Err()==nil between the delete and the cancel call.
func (h *deviceHub) unsubscribe(id int) uint64 {
	h.mu.Lock()
	drops := h.subDrops[id]
	delete(h.subs, id)
	delete(h.subDrops, id)
	delete(h.subExplicit, id)
	if len(h.subs) == 0 {
		h.cancel()
	}
	h.mu.Unlock()
	return drops
}

// heldExplicitly reports whether any current subscriber asserted this hub's
// stream parameters explicitly, and the label of one such holder (the one
// with the lowest subscriber id, for determinism). A hub whose explicit
// consumers have all left is held only by parameter-less subscribers, so its
// parameters are defaulted again no matter what values the producer runs at.
func (h *deviceHub) heldExplicitly() (holder string, held bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	lowest := -1
	for id, label := range h.subExplicit {
		if lowest == -1 || id < lowest {
			lowest, holder = id, label
		}
	}
	return holder, lowest != -1
}

// wasRestarted reports whether this hub's producer was torn down by an
// episode-capture takeover. Subscribers that observe their channel closed
// check it to distinguish "the stream is being restarted at the campaign's
// parameters, reattach" from "the producer stopped, give up".
func (h *deviceHub) wasRestarted() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.restarted
}

// drops reports how many frames the hub has dropped for one subscriber so far.
// A subscriber reads it to turn the running total into a per-sample delta, so a
// gap in sample identifiers always comes with the count that explains it.
func (h *deviceHub) drops(id int) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.subDrops[id]
}

// terminalErr returns the error recorded by runProducer under h.mu.
// Reading h.err must always go through this method: StreamVideo reads h.err
// after receiving from a closed channel, but the close/write ordering in
// runProducer does not provide a happens-before edge visible to the reader
// without an explicit mutex acquisition on the reader side.
func (h *deviceHub) terminalErr() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// maxFrameBytes is the maximum accepted size of a single encoded video frame.
// Frames larger than this are dropped before distribution to prevent a
// malfunctioning or compromised device from triggering memory exhaustion.
const maxFrameBytes = 2 * 1024 * 1024 // 2 MiB

// frameTooLarge reports whether a frame exceeds what the hub is willing to
// distribute. It is the single place the limit is applied, so the identity
// minted in produce and the delivery done by broadcast can never disagree
// about which frames exist.
func frameTooLarge(frame *videoFrame) bool {
	// Raw capture frames are uncompressed and legitimately far larger than an
	// encoded access unit, so they carry their own ceiling. Applying the
	// encoded limit to them would drop every raw frame silently.
	limit := maxFrameBytes
	if frame.codec == agentpb.VideoCodec_VIDEO_CODEC_RAW {
		limit = maxRawFrameBytes
	}
	return len(frame.data) > limit
}

// produce takes one physical frame from the camera producer, stamps it with its
// harness-wide identity and agent receipt, and delivers it. Every producer path
// funnels through here (see the closure in runProducer), and this is the ONLY
// place a sample identity is minted.
//
// Minting belongs to the arrival of a frame, not to a delivery of it. The hub
// can deliver one physical frame on more than one plane — encoded video to
// viewers, to the sensor subscribers a model app reads, and to episode capture;
// and, where the producer tees it, the uncompressed capture frame to analytic
// subscribers — and each plane is a separate broadcast call. Minting inside
// broadcast made a single camera frame consume one identifier per plane, so the
// encoded stream's identifiers came out [2 4 6] with no drops recorded. That
// breaks two contracts at once: hub_loopback_binding reads a jump in sample_id
// on a dense loopback sequence as a producer-side drop, so every frame read as
// a drop; and the model-input ledger's sample_id is supposed to name the
// episode index entry for the same frame, so the join returned nothing at all.
//
// An oversized frame is dropped here without consuming an identifier, so a gap
// in sample_id always means a sample that existed and was lost, never one that
// never existed.
func (h *deviceHub) produce(frame *videoFrame) bool {
	if frameTooLarge(frame) {
		return true // oversized frame: drop silently, keep the hub alive
	}
	// Stamp the identity and receipt before the frame becomes shared: it is
	// immutable from the moment the first subscriber can see it, and every
	// consumer must read the same values.
	if h.sampleSeq != nil {
		frame.sampleID = h.sampleSeq.Add(1)
	}
	if before, receipt, after, err := data.CaptureReceipt(); err == nil {
		frame.receiptBootNanos, frame.receiptUncertaintyNanos = receipt, (after-before+1)/2
	}
	return h.broadcast(frame)
}

// broadcast delivers a frame to the subscribers that want its kind — encoded
// video to viewers, VIDEO_CODEC_RAW frames to raw subscribers — dropping for
// slow consumers. Returns false when there are no subscribers left (producer
// should stop). Late-joining subscribers receive whatever frame the producer
// sends next; they will not see an IDR/keyframe until the next one arrives
// naturally (at most one GOP interval away for GStreamer pipelines with
// key-int-max set).
//
// This is delivery only: it mints nothing. Call produce for a frame arriving
// from the producer, and broadcast to put an existing frame onto a further
// plane. A second plane must never consume a second identifier for the same
// physical frame.
//
// Sends are performed while holding h.mu so that runProducer cannot close
// subscriber channels concurrently — sending on a closed channel panics. With
// maxSubscribersPerHub = 16 and non-blocking selects, the lock is held for
// O(16) nanoseconds, making the contention cost negligible.
func (h *deviceHub) broadcast(frame *videoFrame) bool {
	raw := frame.codec == agentpb.VideoCodec_VIDEO_CODEC_RAW
	if frameTooLarge(frame) {
		return true // oversized frame: drop silently, keep the hub alive
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) == 0 {
		return false
	}
	for id, sub := range h.subs {
		if sub.closed || sub.raw != raw {
			continue
		}
		select {
		case sub.ch <- frame:
		default:
			h.subDrops[id]++
		}
	}
	return true
}

// wantRaw reports whether any live subscriber is waiting for raw frames, so the
// raw tap can skip the per-frame copy when nobody is.
func (h *deviceHub) wantRaw() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, sub := range h.subs {
		if sub.raw && !sub.closed {
			return true
		}
	}
	return false
}

// publishRaw hands one raw capture frame to the hub's raw subscribers.
func (h *deviceHub) publishRaw(data []byte, tsNs uint64, format *agentpb.RawFormat) bool {
	return h.broadcast(&videoFrame{data: data, tsNs: tsNs, codec: agentpb.VideoCodec_VIDEO_CODEC_RAW, rawFmt: format})
}

// rawOffered records that this producer tees raw frames of the given layout.
func (h *deviceHub) rawOffered(format *agentpb.RawFormat) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rawState = rawAvailable
	h.rawFormat = format
}

// rawNotOffered records that this producer has no raw frames to give, and turns
// away every raw subscriber waiting on it with the reason. Their channels are
// closed here, under h.mu, so broadcast and the final teardown skip them; the
// subscriber's own StreamVideo call still removes it. A raw subscriber left
// waiting on a stream that will never deliver is exactly the silent failure
// this whole feature exists to avoid.
func (h *deviceHub) rawNotOffered(reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rawState = rawUnavailable
	h.rawReason = reason
	for _, sub := range h.subs {
		if sub.raw && !sub.closed {
			sub.err = errRawUnavailable(reason)
			sub.closed = true
			close(sub.ch)
		}
	}
}

// rawRefusal returns the up-front refusal for a raw subscriber, or nil while raw
// is offered or still undecided.
func (h *deviceHub) rawRefusal() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rawState == rawUnavailable {
		return errRawUnavailable(h.rawReason)
	}
	return nil
}

// videoSourceKind distinguishes physical/local V4L2, network, and ROS 2 cameras.
type videoSourceKind int

const (
	sourceV4L2 videoSourceKind = iota
	sourceIP
	sourceROS2
)

// videoSource is what a device ID resolves to. Introducing it removes the
// assumption, previously baked into a fmt.Sprintf in StreamVideo, that every
// device ID names a /dev/videoN node.
type videoSource struct {
	kind   videoSourceKind
	key    string // hub map key: "/dev/video0" or "ip:200"
	path   string // V4L2 node path; empty for a network camera
	camera ipcam.Camera
}

// ipHubKeyPrefix marks a hub key as belonging to a network camera. Hub keys are
// opaque strings, so the two kinds share one map.
const ipHubKeyPrefix = "ip:"

// reasonIPCameraNoCredentials is the ErrorInfo reason for a network camera with
// no stored login. The command-line interface turns it into a camera login hint,
// the same way TEGRA_FIRMWARE_MISMATCH becomes an os install hint.
const reasonIPCameraNoCredentials = streamreason.IPCameraNoCredentials

// State paths for virtual cameras, under the agent's existing state directory.
// Declared as vars so tests can point them at a temporary directory.
var (
	ipcamRegistryPath      = "/var/lib/wendy/cameras.json"
	ipcamCredentialPath    = "/var/lib/wendy/camera-credentials.json"
	ros2CameraRegistryPath = "/var/lib/wendy/ros2-cameras.json"
)

// cameraLoopback is the seam VideoService uses to reach the v4l2loopback node
// manager (ipcam.Loopback): every method here matches ipcam.Loopback's
// exported API verbatim, so *ipcam.Loopback satisfies it unmodified, while
// tests inject a fake that records calls instead of touching the kernel
// module. The field this backs is nil-safe at every call site, the same way
// registry/credentials are, so a VideoService built without one (or with it
// cleared out from under it, as a test may do) behaves exactly as it did
// before the v4l2loopback manager existed.
type cameraLoopback interface {
	Available() error
	EnsureNodes(ctx context.Context) error
	NodePath(camID uint32) (string, bool)
	AcquireView(camID uint32) func()
	SetContainerConsumers(containerIDs []string)
	CredentialsChanged(camID uint32)
	RemoveCamera(camID uint32)
	EnsureNode(ctx context.Context, id uint32, label string) error
	Shutdown()
	// Auxiliary nodes back the two-plane camera data path: v4l2loopback nodes
	// fed from the producer hub rather than from a registered network camera.
	// See video_service_two_plane.go.
	AllocateAuxNodeNumber() (int, error)
	EnsureAuxNode(ctx context.Context, nr int, label string) error
	RemoveAuxNode(nr int)
}

// VideoService implements agentpb.WendyVideoServiceServer.
type VideoService struct {
	agentpb.UnimplementedWendyVideoServiceServer
	logger          *zap.Logger
	globDevices     func() ([]string, error)
	readDeviceName  func(base string) (string, error)
	hasVideoCapture func(path string) bool
	// readStableNames returns the udev by-id / by-path names keyed by resolved
	// device path. Injectable so the enumeration can be tested without
	// /dev/v4l. See video_stable_id.go for why these exist.
	readStableNames func() map[string]stableNames

	// Network camera state. registry and credentials are nil-safe throughout: a
	// device that has never seen a network camera behaves exactly as before.
	registry    *ipcam.Registry
	credentials *ipcam.CredentialStore
	discoverer  *ipcam.Discoverer
	links       *ipcam.LinkManager
	loopback    cameraLoopback
	ros2Cameras *ros2camera.Manager

	// twoPlaneState holds the two-plane camera data path's nodes and pumps.
	twoPlaneState

	// runGStreamer is the injection seam for the network capture subprocess.
	runGStreamer func(ctx context.Context, args []string, onFrame func([]byte)) error

	// cameraReachable tests a camera's RTSP port before a stream is started.
	// Injectable so preflight is testable without a socket.
	cameraReachable func(address string) bool

	// probeCamera validates a network camera's stored credentials by dialing
	// its RTSP port directly (see ipcam.ProbeCredentials). It backs
	// TestCameraCredentials; injectable so that RPC is testable without a
	// socket, the same way cameraReachable is for StreamVideo's preflight.
	probeCamera func(ctx context.Context, cam ipcam.Camera, cred ipcam.Credential) (ipcam.ProbeResult, string)

	// CSI/ribbon-camera seams (injectable for tests). classifyTransport maps a
	// /dev/videoN base to its transport (USB/CSI/Unknown); enumerateLibcamera
	// lists libcamera-visible cameras; isJetson selects the Argus capture path.
	classifyTransport  func(base string) (camera.Transport, string)
	enumerateLibcamera func(ctx context.Context) (map[string]string, error)
	isJetson           func() bool
	// findCameraSource resolves a device path to its PipeWire node serial. A
	// seam so tests do not fork pw-dump at the host's session.
	findCameraSource func(ctx context.Context, devicePath string) (uint64, bool)
	readTegraRelease func() ([]byte, error)
	dumpBootSlots    func(context.Context) ([]byte, error)

	// startProducer launches the capture loop that feeds a hub. It is the
	// injection seam for hub lifecycle tests (creation, capture takeover): the
	// default forks runProducer, and a test substitutes a producer that
	// broadcasts frames without opening a device. Any substitute must perform
	// runProducer's teardown duties when its context ends: evict the hub from
	// s.hubs, close the subscriber channels, and close h.done.
	startProducer func(ctx context.Context, h *deviceHub, path string, req *agentpb.StreamVideoRequest)

	// Tunable V4L2 controls (exposure/gain/white balance) for local cameras.
	// controls persists desired values so they survive a pipeline reopen and an
	// agent restart; applyLocalControls/queryLocalControls are seams so the two
	// RPCs are testable without a real /dev/video node (see camera_controls.go).
	controls           *cameraControlStore
	applyLocalControls func(path string, ctrls []controlValue) ([]*agentpb.CameraControlResult, error)
	queryLocalControls func(path string) ([]*agentpb.CameraControl, error)
	// controlIndexFor resolves a control name to its CID by asking the camera.
	// A seam for the same reason as the two above: the tests must not need a
	// real /dev/videoN to exercise name resolution.
	controlIndexFor func(path string) (map[string]uint32, error)
	// controlDefaultsFor reads the driver's own default for each named control.
	// A seam for the same reason: ResetCameraControls has to ask the hardware
	// what "default" means, and its behaviour -- including that it forgets a
	// stored control even when the write fails -- must be testable without one.
	controlDefaultsFor func(path string, names []string) (map[string]int32, error)

	ctx    context.Context    // cancelled on Shutdown; hub contexts are derived from this
	cancel context.CancelFunc // cancels ctx
	wg     sync.WaitGroup     // tracks active runProducer goroutines

	mu   sync.Mutex
	hubs map[string]*deviceHub
	// sampleSeqs holds one sample counter per device key, outliving the hubs so
	// a producer restart does not reissue sample identities the harness has
	// already handed out for that source.
	sampleSeqs map[string]*atomic.Uint64
}

// NewVideoService creates a VideoService whose producer goroutines are tied to ctx.
// Call Shutdown to cancel all active producers and wait for them to exit.
func NewVideoService(ctx context.Context, logger *zap.Logger, rosRuntime ...ROS2Runtime) *VideoService {
	svcCtx, cancel := context.WithCancel(ctx)
	svc := &VideoService{
		logger:     logger,
		ctx:        svcCtx,
		cancel:     cancel,
		hubs:       make(map[string]*deviceHub),
		sampleSeqs: make(map[string]*atomic.Uint64),
		globDevices: func() ([]string, error) {
			return filepath.Glob("/dev/video*")
		},
		readDeviceName: func(base string) (string, error) {
			b, err := os.ReadFile(fmt.Sprintf("/sys/class/video4linux/%s/name", base))
			return strings.TrimSpace(string(b)), err
		},
		hasVideoCapture: func(path string) bool {
			// O_RDONLY is sufficient for VIDIOC_QUERYCAP (read-only ioctl).
			// Using O_RDWR requests unnecessary write privilege and can cause EBUSY
			// on exclusive-access cameras that reject a second writable open.
			fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				return false
			}
			defer unix.Close(fd) //nolint:errcheck
			var cap v4l2Capability
			_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocQueryCap, uintptr(unsafe.Pointer(&cap)))
			return errno == 0 && cap.hasVideoCapture()
		},
		readStableNames: func() map[string]stableNames {
			return readStableNames(v4lByIDDir, v4lByPathDir)
		},
		classifyTransport:  camera.Classify,
		enumerateLibcamera: camera.EnumerateLibcamera,
		findCameraSource:   audio.FindCameraSource,
		isJetson:           func() bool { return board.Detect().IsJetson() },
		readTegraRelease:   func() ([]byte, error) { return os.ReadFile("/etc/nv_tegra_release") },
		dumpBootSlots: func(ctx context.Context) ([]byte, error) {
			return exec.CommandContext(ctx, "nvbootctrl", "dump-slots-info").CombinedOutput()
		},
		registry:    ipcam.NewRegistry(ipcamRegistryPath),
		credentials: ipcam.NewCredentialStore(ipcamCredentialPath),
	}

	// Load persisted network cameras. A failure here must not stop the video
	// service: local cameras still work without it.
	if err := svc.registry.Load(); err != nil {
		logger.Warn("loading network camera registry failed", zap.Error(err))
	}
	if err := svc.credentials.Load(); err != nil {
		logger.Warn("loading network camera credentials failed", zap.Error(err))
	}
	svc.controls = newCameraControlStore(cameraControlsPath)
	svc.applyLocalControls = applyCameraControlsV4L2
	svc.controlDefaultsFor = cameraControlDefaults
	svc.queryLocalControls = queryCameraControlsV4L2
	svc.controlIndexFor = cameraControlIndex
	if err := svc.controls.Load(); err != nil {
		logger.Warn("loading camera controls failed", zap.Error(err))
	}
	svc.discoverer = ipcam.NewDiscoverer(svc.registry, logger)
	svc.links = ipcam.NewLinkManager(svc.registry, logger)
	svc.runGStreamer = svc.gstreamerFrames
	svc.cameraReachable = ipcam.Reachable
	svc.probeCamera = ipcam.ProbeCredentials
	svc.startProducer = func(ctx context.Context, h *deviceHub, path string, req *agentpb.StreamVideoRequest) {
		svc.wg.Add(1)
		go svc.runProducer(ctx, h, path, req)
	}
	// The pump closure reads svc.runGStreamer through the receiver at call
	// time, not the value assigned above at construction: a test that swaps
	// s.runGStreamer after NewVideoService returns (the usual pattern; see
	// captureIPPipelineArgs) still reaches its stub when the loopback
	// supervisor starts a pump.
	svc.loopback = ipcam.NewLoopback(svcCtx, logger, svc.registry, svc.credentials,
		func(ctx context.Context, args []string) error {
			return svc.runGStreamer(ctx, args, func([]byte) {})
		})
	var graphs ros2camera.GraphSource
	if len(rosRuntime) != 0 && rosRuntime[0] != nil {
		graphs = func(ctx context.Context) ([]ros2camera.Graph, error) {
			targets, err := rosRuntime[0].FindROS2Containers(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]ros2camera.Graph, 0, len(targets))
			for _, target := range targets {
				if target.Running && target.TaskPID != 0 {
					target := target
					key := target.AppID
					if key == "" {
						key = target.ContainerID
					}
					out = append(out, ros2camera.Graph{
						Key: key, InstanceKey: target.ContainerID, DomainID: target.DomainID, NetworkNamespacePID: target.TaskPID,
						Verify: func(ctx context.Context) bool {
							current, err := rosRuntime[0].FindROS2Containers(ctx)
							if err != nil {
								return false
							}
							for _, candidate := range current {
								if candidate.ContainerID == target.ContainerID && candidate.Running && candidate.TaskPID == target.TaskPID {
									return true
								}
							}
							return false
						},
					})
				}
			}
			return out, nil
		}
	}
	svc.ros2Cameras = ros2camera.NewManager(svcCtx, logger, svc.loopback, ros2CameraRegistryPath, graphs)
	return svc
}

// discoveryInterval is how often the agent re-probes for network cameras. It is
// deliberately unhurried: a round is one multicast probe plus an ARP read, and
// cameras do not come and go quickly.
const discoveryInterval = 60 * time.Second

// StartDiscovery runs discovery rounds and camera-link management until the
// service context is cancelled. Call once at agent start.
//
// The link manager is what makes a directly-cabled camera work: such a camera
// holds no address at all, because its segment has no DHCP server, so it never
// answers a discovery probe. See ipcam.LinkGuard for the conditions under which
// the agent is willing to be that server.
func (s *VideoService) StartDiscovery() {
	if s.ros2Cameras != nil {
		s.ros2Cameras.Start()
	}
	if s.links != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.links.Run(s.ctx)
		}()
	}
	if s.discoverer == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(discoveryInterval)
		defer ticker.Stop()
		for {
			if _, err := s.discoverer.Once(s.ctx); err != nil {
				s.logger.Debug("camera discovery round failed", zap.Error(err))
			}
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Shutdown cancels all active producer goroutines and waits for them to exit.
// The loopback manager is shut down first, and through its own Shutdown
// method rather than only by cancelling the shared context it was built on:
// that method sets its shuttingDown flag before cancelling and waiting (see
// ipcam.Loopback's doc comment on the field), which guarantees no pump or
// idle-grace goroutine can start after this call begins. Cancelling s.ctx
// first would race that guarantee — a reconcile already past its own
// shuttingDown check could still start a new supervisor moments after ctx
// cancellation fires but before Loopback.Shutdown gets a chance to run.
func (s *VideoService) Shutdown() {
	// Two-plane pumps go first, and are waited for, because each one holds a hub
	// subscription and an open v4l2loopback node. Cancelling s.ctx alone would
	// stop them eventually but would not wait, so a pump could still be
	// releasing its node after Shutdown returned.
	s.stopAllTwoPlane()
	if s.ros2Cameras != nil {
		s.ros2Cameras.Shutdown()
	}
	if s.loopback != nil {
		s.loopback.Shutdown()
	}
	s.cancel()
	s.wg.Wait()
}

// listCameras enumerates every camera the device can offer: local V4L2 nodes
// first, then registered network cameras.
func (s *VideoService) listCameras(ctx context.Context) ([]*agentpb.VideoDevice, error) {
	paths, err := s.globDevices()
	if err != nil {
		return nil, err
	}
	libcameraIDs, libErr := s.enumerateLibcamera(ctx)
	if libErr != nil {
		// Enumeration errors are non-fatal — we just lose the libcamera id enrichment.
		s.logger.Debug("libcamera enumeration failed", zap.Error(libErr))
	}
	// Read once for the whole listing rather than per device: it is two
	// directory walks, and a camera appearing or vanishing midway through would
	// otherwise give some entries an identity and not others.
	stable := map[string]stableNames{}
	if s.readStableNames != nil {
		stable = s.readStableNames()
	}
	var (
		devices       []*agentpb.VideoDevice
		csiDeviceIdxs []int
	)
	for _, path := range paths {
		base := filepath.Base(path)
		numStr := strings.TrimPrefix(base, "video")
		id, err := strconv.ParseUint(numStr, 10, 32)
		if err != nil {
			continue
		}
		// A v4l2loopback node lives at /dev/video<cameraID>, numbered from the
		// same reserved band resolveSource treats as a network camera. Once
		// EnsureNodes has created one, it would otherwise glob-enumerate here
		// too and double-list the camera: once (correctly) from listIPCameras
		// below and once (bogusly) as an indistinguishable local device.
		if id >= uint64(ipcam.LoopbackBandStart) && id <= uint64(ipcam.IDBandEnd) {
			continue
		}
		if !s.hasVideoCapture(path) {
			continue
		}
		name, err := s.readDeviceName(base)
		if err != nil {
			name = base
		}
		transport, driver := s.classifyTransport(base)
		// Skip non-camera m2m nodes (Pi 0-4 bcm2835-isp/codec) that advertise
		// VIDEO_CAPTURE but are not capture sources (WDY-1603).
		if camera.IsNonCameraDriver(driver) {
			continue
		}
		dev := &agentpb.VideoDevice{
			Id:        uint32(id),
			Name:      name,
			Path:      path,
			Transport: transportToProto(transport),
			Driver:    driver,
		}
		// Empty for a camera with no /dev/v4l entry, which is not an error --
		// the numeric id still addresses it, it is just not stable across a
		// reboot.
		dev.StableId = stable[path].stableID()
		if transport == camera.TransportCSI {
			csiDeviceIdxs = append(csiDeviceIdxs, len(devices))
		}
		devices = append(devices, dev)
	}
	// Only assign a libcamera_id in the unambiguous single-CSI / single-libcamera
	// case. With multiple cameras the /dev/videoN ↔ libcamera-name mapping is
	// fragile across libcamera versions, so we leave the field empty and let
	// libcamerasrc auto-select at capture time.
	if len(csiDeviceIdxs) == 1 && len(libcameraIDs) == 1 {
		for id := range libcameraIDs {
			devices[csiDeviceIdxs[0]].LibcameraId = id
		}
	}
	devices = append(devices, s.listROS2Cameras()...)
	devices = append(devices, s.listIPCameras()...)
	return devices, nil
}

func (s *VideoService) listROS2Cameras() []*agentpb.VideoDevice {
	if s.ros2Cameras == nil {
		return nil
	}
	cameras := s.ros2Cameras.List()
	out := make([]*agentpb.VideoDevice, 0, len(cameras))
	for _, cam := range cameras {
		out = append(out, &agentpb.VideoDevice{
			Id: cam.ID, Name: cam.Name, Path: cam.Path,
			Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_ROS2,
			Topic:     cam.Topic, Online: true,
		})
	}
	return out
}

// listIPCameras renders registered network cameras as VideoDevices. Path stays
// empty until a loopback node exists, so the listing never names a node the
// caller cannot open.
func (s *VideoService) listIPCameras() []*agentpb.VideoDevice {
	if s.registry == nil {
		return nil
	}
	cameras := s.registry.List()
	out := make([]*agentpb.VideoDevice, 0, len(cameras))
	for _, c := range cameras {
		name := c.Model
		if name == "" {
			name = "network camera"
		}
		dev := &agentpb.VideoDevice{
			Id:             c.ID,
			Name:           name,
			Transport:      agentpb.VideoTransport_VIDEO_TRANSPORT_IP,
			Address:        c.Address,
			Model:          c.Model,
			Mac:            c.MAC,
			HasCredentials: s.credentials != nil && s.credentials.Has(c.MAC),
			Online:         c.Online,
		}
		if s.loopback != nil {
			if path, ok := s.loopback.NodePath(c.ID); ok {
				dev.Path = path
			}
		}
		out = append(out, dev)
	}
	return out
}

func (s *VideoService) ListVideoDevices(ctx context.Context, _ *agentpb.ListVideoDevicesRequest) (*agentpb.ListVideoDevicesResponse, error) {
	devices, err := s.listCameras(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to enumerate video devices: %v", err)
	}
	return &agentpb.ListVideoDevicesResponse{Devices: devices}, nil
}

// resolveSource maps a device ID onto the thing it names. IDs in the network
// camera band resolve through the registry; everything else is a V4L2 node.
func (s *VideoService) resolveSource(devID uint32) (videoSource, error) {
	if devID >= ros2camera.IDBandStart && devID <= ros2camera.IDBandEnd {
		if s.ros2Cameras == nil {
			return videoSource{}, status.Errorf(codes.NotFound, "camera %d not found", devID)
		}
		cam, ok := s.ros2Cameras.Get(devID)
		if !ok {
			return videoSource{}, status.Errorf(codes.NotFound, "camera %d not found", devID)
		}
		return videoSource{kind: sourceROS2, key: fmt.Sprintf("ros2:%d", devID), path: cam.Path}, nil
	}
	if devID >= ipcam.IDBandStart && devID <= ipcam.IDBandEnd {
		if s.registry == nil {
			return videoSource{}, status.Errorf(codes.NotFound, "camera %d not found", devID)
		}
		cam, ok := s.registry.Get(devID)
		if !ok {
			// An unregistered ID in the band must not fall through to opening
			// /dev/video<id>, which could be an unrelated node.
			return videoSource{}, status.Errorf(codes.NotFound, "camera %d not found", devID)
		}
		return videoSource{
			kind:   sourceIP,
			key:    fmt.Sprintf("%s%d", ipHubKeyPrefix, devID),
			camera: cam,
		}, nil
	}
	path := fmt.Sprintf("/dev/video%d", devID)
	return videoSource{kind: sourceV4L2, key: path, path: path}, nil
}

// SetCameraCredentials stores the login for a network camera. The secret is
// write-only: no RPC returns it.
func (s *VideoService) SetCameraCredentials(_ context.Context, req *agentpb.SetCameraCredentialsRequest) (*agentpb.SetCameraCredentialsResponse, error) {
	if s.registry == nil || s.credentials == nil {
		return nil, status.Error(codes.Unavailable, "network camera support unavailable")
	}
	cam, ok := s.registry.Get(req.GetDeviceId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "camera %d not found", req.GetDeviceId())
	}
	if req.GetUsername() == "" {
		return nil, status.Error(codes.InvalidArgument, "username is required")
	}
	if err := s.credentials.Set(cam.MAC, ipcam.Credential{
		Username: req.GetUsername(),
		Password: req.GetPassword(),
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "storing credentials: %v", err)
	}
	// Nudge the loopback supervisor: a pump already demanded but blocked on
	// missing credentials can start now, and a running one picks up changed
	// credentials on its next attempt rather than an existing connection with
	// the old login silently going stale.
	if s.loopback != nil {
		s.loopback.CredentialsChanged(cam.ID)
	}
	return &agentpb.SetCameraCredentialsResponse{}, nil
}

// ForgetCamera removes a network camera and its stored credentials.
func (s *VideoService) ForgetCamera(_ context.Context, req *agentpb.ForgetCameraRequest) (*agentpb.ForgetCameraResponse, error) {
	if s.registry == nil {
		return nil, status.Error(codes.Unavailable, "network camera support unavailable")
	}
	cam, ok := s.registry.Get(req.GetDeviceId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "camera %d not found", req.GetDeviceId())
	}
	// Credentials go first: a camera left registered without them is recoverable
	// with camera login, whereas an orphaned credential is invisible.
	if s.credentials != nil {
		if err := s.credentials.Delete(cam.MAC); err != nil {
			return nil, status.Errorf(codes.Internal, "removing credentials: %v", err)
		}
	}
	if !s.registry.Forget(req.GetDeviceId()) {
		return nil, status.Errorf(codes.Internal, "removing camera %d failed", req.GetDeviceId())
	}
	if s.loopback != nil {
		s.loopback.RemoveCamera(cam.ID)
	}
	return &agentpb.ForgetCameraResponse{}, nil
}

// EnsureCameraNodes creates a v4l2loopback node for every registered network
// camera that does not already have one. It is a pass-through to the loopback
// manager, exported for the containerd-side camera provider (Task C6) to call
// before entitling a container to `/dev/video*`; nil-safe like every other
// loopback call site, so a build without the module just does nothing.
func (s *VideoService) EnsureCameraNodes(ctx context.Context) error {
	if s.loopback == nil {
		return nil
	}
	if err := s.loopback.EnsureNodes(ctx); err != nil {
		return err
	}
	if s.ros2Cameras != nil {
		s.ros2Cameras.EnsureNodes(ctx)
	}
	return nil
}

// SetCameraContainerConsumers tells the loopback manager which entitled
// containers are currently running, so it can start or stop camera pumps to
// match. Pass-through for Task C6's containerd-side provider; nil-safe like
// every other loopback call site.
func (s *VideoService) SetCameraContainerConsumers(ctx context.Context, containerIDs []string) {
	if s.loopback == nil {
		return
	}
	s.loopback.SetContainerConsumers(containerIDs)
	if s.ros2Cameras != nil {
		s.ros2Cameras.SetContainerConsumers(containerIDs)
	}
}

// RefreshCameras runs one discovery round and returns the full camera listing.
func (s *VideoService) RefreshCameras(ctx context.Context, _ *agentpb.RefreshCamerasRequest) (*agentpb.RefreshCamerasResponse, error) {
	if s.discoverer != nil {
		if _, err := s.discoverer.Once(ctx); err != nil {
			// A failed probe still leaves previously discovered cameras listable.
			s.logger.Warn("camera discovery failed", zap.Error(err))
		}
	}
	if s.ros2Cameras != nil {
		s.ros2Cameras.Refresh(ctx)
	}
	devices, err := s.listCameras(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing cameras: %v", err)
	}
	return &agentpb.RefreshCamerasResponse{Devices: devices}, nil
}

// TestCameraCredentials validates a network camera's stored login by probing
// its RTSP port directly (ipcam.ProbeCredentials), without starting a capture
// pipeline. It exists as a dedicated RPC rather than reusing StreamVideo's
// preflight because that preflight only distinguishes reachable from
// unreachable — actually learning "bad password" would mean starting a hub
// and its GStreamer pipeline just to observe a 401, conflating a credentials
// problem with a pipeline-failure one. Validation runs device-side because
// the camera is only reachable from the device's own link, which the CLI may
// be remote from over the tunnel.
//
// OK/AUTH_FAILED/UNREACHABLE are all returned as ordinary response data, not
// gRPC errors: each is an expected, actionable outcome of a credentials test.
// The one exception is a camera with no stored login at all, which reuses
// ipCameraCredentials verbatim (FailedPrecondition + ErrorInfo{Reason:
// IP_CAMERA_NO_CREDENTIALS}) so cameraStreamDiagnostic on the CLI side prints
// the established `camera login` hint with zero new mapping.
func (s *VideoService) TestCameraCredentials(ctx context.Context, req *agentpb.TestCameraCredentialsRequest) (*agentpb.TestCameraCredentialsResponse, error) {
	src, err := s.resolveSource(req.GetDeviceId())
	if err != nil {
		return nil, err
	}
	if src.kind != sourceIP {
		return nil, status.Errorf(codes.InvalidArgument,
			"camera %d is a local camera; it has no stored credentials to test", req.GetDeviceId())
	}
	cred, err := s.ipCameraCredentials(src.camera)
	if err != nil {
		return nil, err
	}
	result, detail := s.probeCamera(ctx, src.camera, cred)
	return &agentpb.TestCameraCredentialsResponse{
		Result:  probeResultToProto(result),
		Address: src.camera.Address,
		Detail:  detail,
	}, nil
}

// probeResultToProto maps ipcam.ProbeCredentials' outcome onto the wire enum.
// Every ipcam.ProbeResult value has a case; the default only guards against a
// future ProbeResult this switch has not been updated for.
func probeResultToProto(r ipcam.ProbeResult) agentpb.TestCameraCredentialsResponse_Result {
	switch r {
	case ipcam.ProbeOK:
		return agentpb.TestCameraCredentialsResponse_RESULT_OK
	case ipcam.ProbeAuthFailed:
		return agentpb.TestCameraCredentialsResponse_RESULT_AUTH_FAILED
	case ipcam.ProbeUnreachable:
		return agentpb.TestCameraCredentialsResponse_RESULT_UNREACHABLE
	default:
		return agentpb.TestCameraCredentialsResponse_RESULT_UNSPECIFIED
	}
}

// ipCameraCredentials returns the stored login, or a FailedPrecondition carrying
// reasonIPCameraNoCredentials so the client can print the fix.
func (s *VideoService) ipCameraCredentials(cam ipcam.Camera) (ipcam.Credential, error) {
	if s.credentials != nil {
		if cred, ok := s.credentials.Get(cam.MAC); ok {
			return cred, nil
		}
	}
	return ipcam.Credential{}, streamreason.New(codes.FailedPrecondition,
		fmt.Sprintf("camera %d has no stored credentials", cam.ID),
		reasonIPCameraNoCredentials, map[string]string{"device_id": fmt.Sprintf("%d", cam.ID)})
}

// maxHubRetries is the maximum number of times getOrCreateHub will retry after
// observing a race between subscribe() and the last subscriber's cancel() call.
// Exceeding this indicates pathological churn and we return Unavailable.
// hubTeardownTimeout caps how long each retry waits for a dying hub to release
// the device fd, bounding the worst-case total wait to maxHubRetries * timeout.
const (
	maxHubRetries      = 3
	hubTeardownTimeout = 500 * time.Millisecond
)

// hubHolder labels name the kind of consumer whose join asserted a hub's
// stream parameters, so a refused campaign capture can be told exactly who
// holds the camera. They appear verbatim in manifest notes.
const (
	hubHolderStreamClient   = "a video stream client"
	hubHolderEpisodeCapture = "episode capture"
)

// streamRequestAssertsParams reports whether a stream request asserts any
// stream parameter explicitly. A request with no width, height, or framerate
// is the parameter-less join used by consumers that must never take a running
// stream away from anyone (the sensor socket, the loopback pump).
func streamRequestAssertsParams(req *agentpb.StreamVideoRequest) bool {
	return req.GetWidth() != 0 || req.GetHeight() != 0 || req.GetFramerate() != 0
}

// getOrCreateHub returns the existing hub for path, or starts a new producer and hub.
// The caller receives a hub with at least one subscriber already registered (the returned id/ch).
// Returns an error if a hub already exists with different stream parameters.
// origin labels the kind of consumer joining (see the hubHolder constants); it
// is recorded against the subscription only when req asserts parameters
// explicitly.
func (s *VideoService) getOrCreateHub(ctx context.Context, path string, req *agentpb.StreamVideoRequest, origin string) (h *deviceHub, id int, ch chan *videoFrame, err error) {
	explicitHolder := ""
	if streamRequestAssertsParams(req) {
		explicitHolder = origin
	}
	for retries := 0; ; retries++ {
		if retries >= maxHubRetries {
			s.logger.Warn("hub retry limit exceeded", zap.String("device", path), zap.Int("retries", retries))
			return nil, 0, nil, status.Errorf(codes.Unavailable, "video device temporarily unavailable, please retry")
		}
		s.mu.Lock()
		h, exists := s.hubs[path]
		if !exists {
			break
		}
		if h.ctx.Err() == nil {
			// A caller that names a size must get it or a refusal — handing 640x480
			// to a viewer that asked for 1080p would be worse than saying no. A
			// caller that names nothing is asking for whatever the camera is doing,
			// and what the camera is doing is what is already playing: a bare
			// `camera view` joins an app that pinned the thermal module's raw mode
			// instead of being locked out of its own camera.
			if !requestsDeviceDefault(req) &&
				(h.width != req.GetWidth() || h.height != req.GetHeight() || h.framerate != req.GetFramerate()) {
				s.mu.Unlock()
				s.logger.Debug("stream parameter mismatch", zap.String("device", path),
					zap.Uint32("existing_w", h.width), zap.Uint32("existing_h", h.height),
					zap.Uint32("existing_fps", h.framerate))
				return nil, 0, nil, status.Errorf(codes.InvalidArgument, "device already in use with different stream parameters")
			}
			if req.GetCodec() == agentpb.VideoCodec_VIDEO_CODEC_RAW {
				if refusal := h.rawRefusal(); refusal != nil {
					s.mu.Unlock()
					return nil, 0, nil, refusal
				}
			}
			id, ch, err = h.subscribeAsKind(explicitHolder, req.GetCodec() == agentpb.VideoCodec_VIDEO_CODEC_RAW)
			s.mu.Unlock()
			if err != nil {
				if st, _ := status.FromError(err); st.Code() == codes.Unavailable {
					// subscribe() detected a cancelled hub atomically under h.mu.
					// Wait for the producer to release the device fd, evict the stale
					// hub, and retry so we create a fresh one.
					waitCtx, waitCancel := context.WithTimeout(ctx, hubTeardownTimeout)
					select {
					case <-h.done:
					case <-ctx.Done():
						waitCancel()
						return nil, 0, nil, ctx.Err()
					case <-waitCtx.Done():
						if ctx.Err() != nil {
							waitCancel()
							return nil, 0, nil, ctx.Err()
						}
						s.logger.Warn("timed out waiting for hub teardown", zap.String("device", path))
					}
					waitCancel()
					s.mu.Lock()
					if s.hubs[path] == h {
						delete(s.hubs, path)
					}
					s.mu.Unlock()
					continue
				}
				return nil, 0, nil, err
			}
			return h, id, ch, nil
		}
		// Hub is cancelling. Evict it and wait for the producer to release
		// the device fd before opening a new one — otherwise VIDIOC_S_FMT
		// returns EBUSY while the old streaming session is still active.
		delete(s.hubs, path)
		done := h.done
		s.mu.Unlock()
		waitCtx, waitCancel := context.WithTimeout(ctx, hubTeardownTimeout)
		select {
		case <-done:
		case <-ctx.Done():
			waitCancel()
			return nil, 0, nil, ctx.Err()
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				waitCancel()
				return nil, 0, nil, ctx.Err()
			}
			s.logger.Warn("timed out waiting for hub teardown", zap.String("device", path))
		}
		waitCancel()
	}
	// s.mu is held here (broke out of loop with no hub in map).

	hctx, cancel := context.WithCancel(s.ctx)
	h = &deviceHub{
		subs:      make(map[int]*hubSubscriber),
		subDrops:  make(map[int]uint64),
		ctx:       hctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		width:     req.GetWidth(),
		height:    req.GetHeight(),
		framerate: req.GetFramerate(),
		sampleSeq: s.sampleSeqLocked(path),
	}
	// New hub: the first subscriber is always within the cap.
	id, ch, _ = h.subscribeAsKind(explicitHolder, req.GetCodec() == agentpb.VideoCodec_VIDEO_CODEC_RAW)
	s.hubs[path] = h
	s.mu.Unlock()

	s.startProducer(hctx, h, path, req)
	return h, id, ch, nil
}

// joinHub subscribes to a device's frame hub without insisting on the stream
// parameters in req. When a hub already runs with different parameters the
// caller must not fail — a live viewer, an episode capture, and a model
// subscriber all legitimately want the same device — so it falls back to
// joining the running hub as it is. Callers that care about the difference
// compare the returned parameters against what they asked for and report it.
func (s *VideoService) joinHub(ctx context.Context, key string, req *agentpb.StreamVideoRequest) (*deviceHub, int, chan *videoFrame, error) {
	hub, id, ch, _, _, _, err := s.joinHubReportingParams(ctx, key, req)
	return hub, id, ch, err
}

// joinHubReportingParams is joinHub, additionally reporting the stream
// parameters the subscription actually runs at.
func (s *VideoService) joinHubReportingParams(ctx context.Context, key string, req *agentpb.StreamVideoRequest) (*deviceHub, int, chan *videoFrame, uint32, uint32, uint32, error) {
	for attempt := 0; attempt < maxHubRetries; attempt++ {
		hub, id, ch, err := s.getOrCreateHub(ctx, key, req, hubHolderStreamClient)
		if err == nil {
			// Report the hub's ACTUAL parameters, never the request's. A
			// request that asserts none now joins whatever the camera is
			// already doing rather than being refused, so echoing the request
			// back here would record an achieved size of 0x0@0 for a capture
			// that is really running at the hub's parameters. For a hub this
			// call created the two are identical, because getOrCreateHub
			// copies them straight from req.
			return hub, id, ch, hub.width, hub.height, hub.framerate, nil
		}
		if status.Code(err) != codes.InvalidArgument {
			return nil, 0, nil, 0, 0, 0, err
		}
		hub, id, ch, w, h, fps, ok, err := s.subscribeExistingHub(key)
		if err != nil {
			return nil, 0, nil, 0, 0, 0, err
		}
		if ok {
			return hub, id, ch, w, h, fps, nil
		}
		// The conflicting hub disappeared between the two calls; retry.
	}
	return nil, 0, nil, 0, 0, 0, status.Error(codes.Unavailable, "video device hub churned during subscription")
}

// errCameraHeldExplicitly is the named refusal for a campaign whose explicit
// capture parameters conflict with a hub that another consumer holds at
// explicitly requested parameters. It is recorded in the episode manifest
// (CaptureResult source detail) instead of silently downgrading the capture;
// the wrapped message names the holder and both parameter sets.
var errCameraHeldExplicitly = errors.New("camera is held at explicitly requested stream parameters")

// joinHubForCapture is the episode-capture join. It implements the parameter
// priority the capture policy promises:
//
//   - A capture that asserts no parameters joins whatever runs, exactly like
//     any other parameter-less subscriber (joinHubReportingParams).
//   - A capture with EXPLICIT parameters wins over a hub whose parameters were
//     chosen by default because every current subscriber asserted none: the
//     producer is restarted at the campaign's parameters and the
//     parameter-less subscribers reattach to the restarted stream
//     (takeOverDefaultedHub). restarted reports that this happened so the
//     capture can note it in the manifest.
//   - A capture with explicit parameters that conflict with a hub held at
//     parameters ANOTHER consumer asserted explicitly is refused with
//     errCameraHeldExplicitly, never silently downgraded.
func (s *VideoService) joinHubForCapture(ctx context.Context, key string, req *agentpb.StreamVideoRequest) (hub *deviceHub, id int, ch chan *videoFrame, width, height, framerate uint32, restarted bool, err error) {
	if !streamRequestAssertsParams(req) {
		hub, id, ch, width, height, framerate, err = s.joinHubReportingParams(ctx, key, req)
		return hub, id, ch, width, height, framerate, false, err
	}
	for attempt := 0; attempt < maxHubRetries; attempt++ {
		hub, id, ch, err := s.getOrCreateHub(ctx, key, req, hubHolderEpisodeCapture)
		if err == nil {
			return hub, id, ch, req.GetWidth(), req.GetHeight(), req.GetFramerate(), false, nil
		}
		if status.Code(err) != codes.InvalidArgument {
			return nil, 0, nil, 0, 0, 0, false, err
		}
		hub, id, ch, retry, err := s.takeOverDefaultedHub(ctx, key, req)
		if err != nil {
			return nil, 0, nil, 0, 0, 0, false, err
		}
		if retry {
			// The conflicting hub disappeared, or its parameters changed,
			// between the two calls; re-evaluate from the top.
			continue
		}
		return hub, id, ch, req.GetWidth(), req.GetHeight(), req.GetFramerate(), true, nil
	}
	return nil, 0, nil, 0, 0, 0, false, status.Error(codes.Unavailable, "video device hub churned during subscription")
}

// takeOverDefaultedHub replaces a hub running at defaulted parameters with one
// running at the campaign's explicit parameters. Returns retry=true (and no
// hub) when the observed hub is gone or no longer conflicts, in which case the
// caller should retry getOrCreateHub; returns errCameraHeldExplicitly when a
// current subscriber holds the hub's parameters explicitly.
//
// Splice semantics, deliberately NOT a mid-stream splice: each existing
// subscriber's stream ends (its channel closes exactly as if the producer had
// stopped) and a new stream begins on the replacement hub. A parameter-less
// subscriber that reattaches gets a fresh decodable start on the new stream,
// beginning on a random-access unit; nothing ever splices two different
// sequence parameter sets into what a downstream decoder reads as one
// timeline. gRPC StreamVideo subscribers get a terminal error telling them to
// reconnect (see pumpFrames).
//
// The replacement hub is installed in s.hubs BEFORE the old producer is
// cancelled, so every reattaching subscriber lands on it rather than racing
// this takeover to create a competing defaulted hub.
func (s *VideoService) takeOverDefaultedHub(ctx context.Context, key string, req *agentpb.StreamVideoRequest) (h *deviceHub, id int, ch chan *videoFrame, retry bool, err error) {
	s.mu.Lock()
	old, exists := s.hubs[key]
	if !exists || old.ctx.Err() != nil {
		s.mu.Unlock()
		return nil, 0, nil, true, nil
	}
	if old.width == req.GetWidth() && old.height == req.GetHeight() && old.framerate == req.GetFramerate() {
		// No conflict any more; the plain subscribe path handles it.
		s.mu.Unlock()
		return nil, 0, nil, true, nil
	}
	if holder, held := old.heldExplicitly(); held {
		w, hgt, fps := old.width, old.height, old.framerate
		s.mu.Unlock()
		return nil, 0, nil, false, fmt.Errorf(
			"%w: %s holds this camera at %s; the campaign requested %s; refusing to capture at parameters the campaign did not ask for",
			errCameraHeldExplicitly, holder,
			describeStreamParams(w, hgt, fps),
			describeStreamParams(req.GetWidth(), req.GetHeight(), req.GetFramerate()))
	}

	hctx, cancel := context.WithCancel(s.ctx)
	h = &deviceHub{
		subs:      make(map[int]*hubSubscriber),
		subDrops:  make(map[int]uint64),
		ctx:       hctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		width:     req.GetWidth(),
		height:    req.GetHeight(),
		framerate: req.GetFramerate(),
		sampleSeq: s.sampleSeqLocked(key),
	}
	id, ch, _ = h.subscribeAs(hubHolderEpisodeCapture)
	s.hubs[key] = h
	old.mu.Lock()
	old.restarted = true
	old.mu.Unlock()
	old.cancel()
	s.mu.Unlock()

	s.logger.Info("episode capture restarting camera producer at campaign parameters",
		zap.String("device", key),
		zap.Uint32("width", req.GetWidth()), zap.Uint32("height", req.GetHeight()),
		zap.Uint32("framerate", req.GetFramerate()))

	// abort dismantles the replacement hub when the caller's context ends
	// before its producer was started. No runProducer exists for it yet, so
	// the cleanup runProducer would normally perform (evict from the map,
	// close subscriber channels, close done) must happen here; otherwise a
	// subscriber that reattached in the interim would wait forever on a
	// channel nothing will ever close.
	abort := func() {
		s.mu.Lock()
		if s.hubs[key] == h {
			delete(s.hubs, key)
		}
		s.mu.Unlock()
		cancel()
		h.mu.Lock()
		for _, sub := range h.subs {
			// Guarded exactly as runProducer's teardown is: a raw subscriber
			// the producer already turned away (rawNotOffered) has had its
			// channel closed under h.mu, and closing it a second time panics.
			if !sub.closed {
				sub.closed = true
				close(sub.ch)
			}
		}
		h.mu.Unlock()
		close(h.done)
	}

	// Wait for the old producer to release the device fd before starting the
	// replacement, exactly like getOrCreateHub's teardown wait; otherwise
	// VIDIOC_S_FMT returns EBUSY while the old streaming session is active.
	waitCtx, waitCancel := context.WithTimeout(ctx, hubTeardownTimeout)
	select {
	case <-old.done:
	case <-ctx.Done():
		waitCancel()
		abort()
		return nil, 0, nil, false, ctx.Err()
	case <-waitCtx.Done():
		if ctx.Err() != nil {
			waitCancel()
			abort()
			return nil, 0, nil, false, ctx.Err()
		}
		s.logger.Warn("timed out waiting for hub teardown before capture takeover", zap.String("device", key))
	}
	waitCancel()

	s.startProducer(hctx, h, key, req)
	return h, id, ch, false, nil
}

// sampleSeqLocked returns the sample counter for a device key, creating it on
// first use. Callers must hold s.mu. Counters are never removed: a device that
// comes back must not restart identities a consumer has already seen.
func (s *VideoService) sampleSeqLocked(path string) *atomic.Uint64 {
	if s.sampleSeqs == nil {
		s.sampleSeqs = make(map[string]*atomic.Uint64)
	}
	seq := s.sampleSeqs[path]
	if seq == nil {
		seq = new(atomic.Uint64)
		s.sampleSeqs[path] = seq
	}
	return seq
}

// subscribeExistingHub joins the live hub for path at whatever stream
// parameters it is already running with, asserting none of its own. It returns
// ok=false (and no error) when no live hub exists or the hub is shutting down,
// in which case the caller should create one via getOrCreateHub. Episode
// capture uses this to coexist with a live viewer instead of failing the
// episode when the viewer's stream parameters differ from the campaign's caps.
func (s *VideoService) subscribeExistingHub(path string) (h *deviceHub, id int, ch chan *videoFrame, width, height, framerate uint32, ok bool, err error) {
	s.mu.Lock()
	h, exists := s.hubs[path]
	if !exists || h.ctx.Err() != nil {
		s.mu.Unlock()
		return nil, 0, nil, 0, 0, 0, false, nil
	}
	id, ch, err = h.subscribe()
	s.mu.Unlock()
	if err != nil {
		if st, _ := status.FromError(err); st.Code() == codes.Unavailable {
			// The hub began shutting down between the map lookup and subscribe.
			return nil, 0, nil, 0, 0, 0, false, nil
		}
		return nil, 0, nil, 0, 0, 0, false, err
	}
	return h, id, ch, h.width, h.height, h.framerate, true, nil
}

// runProducer drives the capture loop for a single device hub.
// It tries native V4L2 H.264 first, falling back to GStreamer when unsupported.
// When the hub loses its last subscriber the context is cancelled and this goroutine exits.
func (s *VideoService) runProducer(ctx context.Context, h *deviceHub, path string, req *agentpb.StreamVideoRequest) {
	defer s.wg.Done()
	broadcast := func(payload []byte, stamp frameTimestamp, codec agentpb.VideoCodec) bool {
		return h.produce(&videoFrame{data: payload, tsNs: stamp.wallNs, codec: codec, nativeNs: stamp.nativeNs, nativeClock: stamp.nativeClock, nativeFlags: stamp.nativeFlags, sequence: stamp.sequence, sequenceValid: stamp.sequenceValid, auAligned: stamp.auAligned})
	}

	// The hub key carries the source kind: network cameras key on "ip:<id>" and
	// have no device node to classify.
	var err error
	if strings.HasPrefix(path, ipHubKeyPrefix) {
		// The camera sends H.264 and the producer depayloads it without ever
		// holding a decoded frame, so there is nothing raw to offer.
		h.rawNotOffered("network cameras deliver encoded video only")
		err = s.runIPProducer(ctx, broadcast, path, req)
	} else {
		transport, _ := s.classifyTransport(filepath.Base(path))
		libcameraID := s.lookupLibcameraID(ctx, transport)

		// Re-assert any persisted exposure/gain before the pipeline opens the
		// node, so a stream reconnect does not revert to the firmware default.
		s.applyStoredCameraControls(path)

		// CSI/ribbon sensors emit raw Bayer/RGB, not encoded H.264 — skip the native
		// V4L2 H.264 path entirely and capture via GStreamer (libcamerasrc, or
		// nvarguscamerasrc on Jetson).
		if transport == camera.TransportCSI {
			s.logger.Info("CSI camera detected, using GStreamer", zap.String("device", path))
			// The ISP pipeline (libcamerasrc / Argus) produces processed video for
			// the encoder; the sensor's own frames never reach the agent as bytes.
			h.rawNotOffered("CSI cameras are captured through the ISP pipeline; raw frames are not offered")
			err = s.streamGStreamer(ctx, broadcast, path, req, transport, libcameraID, pipeWireSource{}, noRawSink{})
		} else {
			err = s.captureLocalCamera(ctx, broadcast, path, req, transport, libcameraID, h)
		}
	}
	if err != nil && ctx.Err() == nil {
		s.logger.Error("video producer exited with error", zap.String("device", path), zap.Error(err))
	}

	// Remove hub so the next StreamVideo call spawns a fresh producer.
	s.mu.Lock()
	if s.hubs[path] == h {
		delete(s.hubs, path)
	}
	s.mu.Unlock()

	// Store the terminal error and close subscriber channels under h.mu.
	// broadcast() also holds h.mu during sends, so closing inside the lock is
	// the synchronisation point that prevents send-on-closed-channel panics:
	// either broadcast() holds h.mu (and finishes its sends before we close),
	// or we hold h.mu first (and close before broadcast() can send).
	h.mu.Lock()
	if err != nil && ctx.Err() == nil {
		h.err = err
	}
	for _, sub := range h.subs {
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}
	h.mu.Unlock()

	// Signal that the device fd is fully released. getOrCreateHub waits on
	// this before opening a new producer to avoid EBUSY on reconnect.
	close(h.done)
}

// captureLocalCamera reads a USB/unknown camera, preferring the cheapest source: raw V4L2
// keeps MJPEG capture and the best-mode probe, so the graph is only worth its
// raw-plus-re-encode cost once the device refuses a second streaming consumer.
func (s *VideoService) captureLocalCamera(ctx context.Context, broadcast func([]byte, frameTimestamp, agentpb.VideoCodec) bool, path string, req *agentpb.StreamVideoRequest, transport camera.Transport, libcameraID string, sink rawSink) error {
	if sink == nil {
		sink = noRawSink{}
	}
	// A second source may only take over before a frame has reached subscribers: restarting
	// mid-stream splices a new resolution and SPS into what downstream reads as one timeline.
	// Plain bool, not atomic: send runs on this producer goroutine's capture loop, and every
	// read below happens after that loop has returned.
	var delivered bool
	// The native path hands us the camera's own H.264 and never sees a decoded
	// frame, so it is the one local source with no raw frames to offer. Its first
	// delivered frame is the proof it is the path in use, and the moment a raw
	// subscriber can be told rather than left waiting.
	nativePhase := true
	send := func(data []byte, stamp frameTimestamp, codec agentpb.VideoCodec) bool {
		ok := broadcast(data, stamp, codec)
		if ok && nativePhase && !delivered {
			sink.rawNotOffered("camera streams H.264 natively; raw frames are not offered")
		}
		delivered = delivered || ok
		return ok
	}

	err := s.streamV4L2Native(ctx, send, path, req)
	nativePhase = false
	if _, ok := err.(nativeH264NotSupported); ok {
		s.logger.Info("native H.264 not supported, falling back to GStreamer", zap.String("device", path))
		err = s.streamGStreamer(ctx, send, path, req, transport, libcameraID, pipeWireSource{}, sink)
	}
	if !isCameraInUse(err) || delivered || ctx.Err() != nil {
		return err
	}

	serial, ok := s.findCameraSource(ctx, path)
	if !ok {
		return err
	}
	s.logger.Info("camera held by another client, capturing through PipeWire",
		zap.String("device", path), zap.Uint64("node_serial", serial))

	// Contention stays the answer when sharing fails outright: it is the cause an operator
	// can act on, and a graph error names a pipeline they did not ask for. Once the shared
	// stream has shipped frames the camera is demonstrably readable, so its own error wins.
	pwErr := s.streamGStreamer(ctx, send, path, req, transport, libcameraID, pipeWireSource{serial: serial}, sink)
	if pwErr == nil || ctx.Err() != nil || isCameraInUse(pwErr) || delivered {
		return pwErr
	}
	s.logger.Warn("PipeWire capture failed for a held camera",
		zap.String("device", path), zap.Error(pwErr))
	return err
}

// gstInspectTimeout bounds the element listing fork.
const gstInspectTimeout = 10 * time.Second

// firstFrameTimeout bounds the wait for a pipeline's first chunk. Generous because it must
// clear the whole cold start: element listing, PipeWire lookup, camera and encoder warm-up.
const firstFrameTimeout = 20 * time.Second

// pipeWireProbeTimeout bounds the format probe below. A raw node yields nothing
// to a JPEG request, so the probe ends by deadline rather than by error.
const pipeWireProbeTimeout = 2 * time.Second

// nodeServesJPEG reports whether the graph is sending MJPEG for this node. pw-dump
// advertises only what the device supports, not what the first client negotiated, and a
// mismatched pipeline stalls rather than fails — so the live format is read, not guessed.
// The second return separates a definite "raw" from an inconclusive probe: a deadline or a
// missing session is not evidence, and treating it as one built a pipeline that stalled.
func (s *VideoService) nodeServesJPEG(ctx context.Context, gstPath string, serial uint64) (jpeg, known bool) {
	probeCtx, cancel := context.WithTimeout(ctx, pipeWireProbeTimeout)
	defer cancel()
	cmd, err := audio.Command(probeCtx, gstPath, "-q",
		"pipewiresrc", fmt.Sprintf("target-object=%d", serial), "num-buffers=1",
		"!", "image/jpeg", "!", "fakesink")
	if err != nil {
		s.logger.Debug("PipeWire format probe could not start",
			zap.Uint64("node_serial", serial), zap.Error(err))
		return false, false
	}
	if runErr := cmd.Run(); runErr != nil {
		// Only a clean refusal is evidence. If our own deadline killed the probe, or the
		// parent context went away, the format is simply unknown.
		if probeCtx.Err() != nil || ctx.Err() != nil {
			s.logger.Debug("PipeWire format probe was inconclusive",
				zap.Uint64("node_serial", serial), zap.Error(runErr))
			return false, false
		}
		return false, true
	}
	return true, true
}

// pipeWireSource names the graph node a pipeline reads, and how.
type pipeWireSource struct {
	serial uint64
	jpeg   bool
}

// maxVideoDeviceID is the upper bound for accepted device IDs.
// Linux's VIDEO_NUM_DEVICES kernel constant (v4l2-dev.h) allows video0–video255.
const maxVideoDeviceID = 255

// v4l2MajorDevice is the Linux character device major number for V4L2 devices
// (documented in Documentation/admin-guide/devices.txt as major 81).
const v4l2MajorDevice = 81

// Absolute bounds on a requested frame size. These are the security property
// validateStreamParams exists for — keeping pipeline arguments bounded — and are
// deliberately generous, because *which* sizes are sensible is the device's
// business, not ours. The lower bound also filters the malformed descriptors
// some UVC firmware advertises (a TOPDON TC001 thermal module reports
// 4x12305, 60x3299 and 8x12578 alongside its real modes).
const (
	minFrameDimension = 16
	maxFrameDimension = 8192
)

// commonFrameSizes is the fallback allowlist, used only when the device cannot
// be enumerated (unplugged, or a non-V4L2 source such as CSI/libcamera).
var commonFrameSizes = [][2]uint32{
	{320, 240}, {640, 480}, {1280, 720}, {1920, 1080}, {3840, 2160},
}

// deviceAdvertisesFrameSize reports whether path enumerates w×h as a discrete
// mode for any pixel format the GStreamer path can negotiate.
//
// VIDIOC_ENUM_FRAMESIZES only needs a read-only open, so this still answers
// while another process is streaming the camera — which is the common case,
// since an app usually holds the device the viewer wants.
func deviceAdvertisesFrameSize(path string, w, h uint32) (advertised, known bool) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, false
	}
	defer unix.Close(fd) //nolint:errcheck

	for _, pixfmt := range []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG} {
		for index := uint32(0); index < 64; index++ {
			fse := v4l2FrmSizeEnum{Index: index, PixelFormat: pixfmt}
			if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocEnumFramesizes,
				uintptr(unsafe.Pointer(&fse))); errno != 0 {
				break // EINVAL marks the end of the list
			}
			if fse.Type != v4l2FrmsizeTypeDiscrete {
				break // stepwise/continuous: nothing discrete to match against
			}
			known = true
			if fse.Width == w && fse.Height == h {
				return true, true
			}
		}
	}
	return false, known
}

// validateStreamParams checks width, height, and framerate before they reach the
// GStreamer pipeline arguments. Zero means "device default" and is always accepted.
//
// A non-zero size is accepted when the DEVICE advertises it. It used to be
// checked against a fixed list of five consumer-webcam modes, which made every
// camera with a non-standard sensor unstreamable at any explicit size: a
// TOPDON TC001 thermal module advertises 256x392, 520x192, 512x484, 644x384 and
// 256x196 — not one of which was on that list — so every explicit request
// returned "unsupported resolution" while the camera was working fine.
//
// devicePath may be empty (or a non-V4L2 source), in which case the old
// allowlist still applies; the absolute bounds are enforced either way.
func validateStreamParams(devicePath string, req *agentpb.StreamVideoRequest) error {
	w, h := req.GetWidth(), req.GetHeight()
	if w != 0 || h != 0 {
		if w == 0 || h == 0 {
			return status.Errorf(codes.InvalidArgument,
				"width and height must be set together")
		}
		if w < minFrameDimension || h < minFrameDimension ||
			w > maxFrameDimension || h > maxFrameDimension {
			return status.Errorf(codes.InvalidArgument,
				"resolution %dx%d out of range (%d..%d per axis)",
				w, h, minFrameDimension, maxFrameDimension)
		}
		advertised, known := false, false
		if devicePath != "" {
			advertised, known = deviceAdvertisesFrameSize(devicePath, w, h)
		}
		if !advertised {
			if known {
				// The device told us its modes and this is not one of them.
				return status.Errorf(codes.InvalidArgument,
					"resolution %dx%d not advertised by this camera", w, h)
			}
			// Could not ask the device — fall back to the historical allowlist.
			if !slices.Contains(commonFrameSizes, [2]uint32{w, h}) {
				return status.Errorf(codes.InvalidArgument, "unsupported resolution")
			}
		}
	}
	fps := req.GetFramerate()
	if fps != 0 {
		switch fps {
		case 15, 24, 25, 30, 60, 90, 120:
		default:
			return status.Errorf(codes.InvalidArgument, "unsupported framerate")
		}
	}
	return nil
}

// streamDeviceID picks the camera this request names.
//
// `stable_id` wins over `device_id` when set, and is resolved HERE -- at
// request time -- rather than by whoever wrote the config. That is the whole
// point of the field: /dev/videoN is assigned in enumeration order at boot, so
// a number written down yesterday may name a different camera today.
//
// A name that matches nothing is an error, deliberately. Falling back to
// `device_id` would stream whatever the kernel happened to put at that number,
// which is exactly the silent wrong-camera failure this field exists to
// remove: opening the wrong camera succeeds, so nobody finds out. An error the
// caller can retry and log beats a plausible picture of the wrong thing.
func (s *VideoService) streamDeviceID(req *agentpb.StreamVideoRequest) (uint32, error) {
	stableID := strings.TrimSpace(req.GetStableId())
	if stableID == "" {
		return req.GetDeviceId(), nil
	}
	if s.readStableNames == nil {
		return 0, status.Errorf(codes.Unimplemented,
			"stable_id is not supported on this agent")
	}
	names := s.readStableNames()
	devID, ok := resolveStableID(names, stableID)
	if !ok {
		return 0, status.Errorf(codes.NotFound,
			"no camera with stable id %q; ListVideoDevices reports %v",
			stableID, knownStableIDs(names))
	}
	return devID, nil
}

// knownStableIDs lists the identities the agent can currently see, for the
// error above. A caller that got the name wrong needs to see the real ones, and
// the alternative is reading agent logs on a device they may not have.
func knownStableIDs(names map[string]stableNames) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if id := n.stableID(); id != "" {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// StreamVideo streams H.264 frames from a V4L2 camera.
// Multiple concurrent callers for the same device share one producer via a deviceHub.
func (s *VideoService) StreamVideo(req *agentpb.StreamVideoRequest, stream grpc.ServerStreamingServer[agentpb.VideoFrame]) error {
	ctx := stream.Context()
	devID, err := s.streamDeviceID(req)
	if err != nil {
		return err
	}
	if devID > maxVideoDeviceID {
		return status.Errorf(codes.InvalidArgument, "device ID out of range")
	}
	src, err := s.resolveSource(devID)
	if err != nil {
		return err
	}

	if src.kind == sourceIP {
		if req.GetCodec() == agentpb.VideoCodec_VIDEO_CODEC_RAW {
			// Refused before a hub exists: the producer would only say the same
			// thing after opening an RTSP session for nothing.
			return errRawUnavailable("network cameras deliver encoded video only")
		}
		// A network camera sends whatever format it is configured for, so the
		// V4L2 resolution allowlist does not apply. Width instead selects which
		// of the camera's streams to open.
		return s.streamIPCamera(stream, src, req)
	}
	if src.kind == sourceROS2 {
		cam, release, err := s.ros2Cameras.Acquire(ctx, devID)
		if err != nil {
			return status.Errorf(codes.Unavailable, "starting ROS 2 camera %d: %v", devID, err)
		}
		defer release()
		src.path = cam.Path
		if src.path == "" {
			return status.Errorf(codes.Unavailable, "ROS 2 camera %d has no loopback device", devID)
		}
	}

	path := src.path
	if err := validateStreamParams(path, req); err != nil {
		return err
	}
	transport, _ := s.classifyTransport(filepath.Base(path))
	if transport == camera.TransportCSI && s.isJetson() {
		if err := s.preflightTegraCSI(ctx); err != nil {
			return err
		}
	}

	// Lstat validates the path before any open: the node must be a character
	// device with V4L2 major number 81 and must not be a symlink. This catches
	// obvious misconfiguration and prevents symlink-based traversal before
	// O_NOFOLLOW is applied at the real open() inside streamV4L2Native.
	// A residual TOCTOU window between this Lstat and the open in streamV4L2Native
	// is unavoidable in the current architecture; O_NOFOLLOW + major-number
	// enforcement together bound it to physical device substitution by a
	// privileged local process, which is outside the threat model for this agent.
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return status.Errorf(codes.NotFound, "video device not found")
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFCHR || unix.Major(uint64(stat.Rdev)) != v4l2MajorDevice {
		return status.Errorf(codes.InvalidArgument, "path is not a V4L2 video device")
	}
	// hasVideoCapture is intentionally not called here: it would open the device
	// a second time before streamV4L2Native opens it, adding an extra TOCTOU
	// window. streamV4L2Native performs VIDIOC_QUERYCAP on the same fd it will
	// use for streaming, so capability verification and streaming happen atomically
	// on a single fd rather than across separate opens.

	h, id, ch, err := s.getOrCreateHub(ctx, path, req, hubHolderStreamClient)
	if err != nil {
		return err
	}
	defer h.unsubscribe(id)

	return s.pumpFrames(stream, h, id, ch)
}

// pumpFrames forwards hub frames to a gRPC stream until the stream or the
// producer ends. Local and network cameras share it, so subscriber teardown and
// terminal-error reporting behave identically for both.
func (s *VideoService) pumpFrames(stream grpc.ServerStreamingServer[agentpb.VideoFrame], h *deviceHub, id int, ch chan *videoFrame) error {
	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame, ok := <-ch:
			if !ok {
				// The producer closed this subscriber alone: raw was asked for and
				// is not offered. That answer belongs to this caller only, so it is
				// checked before the hub-wide terminal error.
				if err := h.subscriberErr(id); err != nil {
					return err
				}
				// Producer exited. Return the original error if one was recorded.
				if err := h.terminalErr(); err != nil {
					return err
				}
				// A capture takeover ends this stream rather than splicing new
				// sequence parameter sets into it mid-timeline; the client
				// reconnects and joins the restarted producer as a new stream.
				if h.wasRestarted() {
					return status.Errorf(codes.Unavailable,
						"video stream ended: episode capture restarted the camera producer at the campaign's requested parameters; reconnect to join the new stream")
				}
				// If the hub context was cancelled (e.g. service shutdown), propagate that.
				if err := h.ctx.Err(); err != nil {
					return status.FromContextError(err).Err()
				}
				return status.Errorf(codes.Internal, "video producer stopped unexpectedly")
			}
			// frame.data is an immutable, per-frame allocation produced by the
			// capture loop and never modified after broadcast(). stream.Send()
			// serialises the proto synchronously (marshal → TLS write) before
			// returning, so passing the shared slice directly is safe and avoids
			// an O(N × frameSize) heap allocation per broadcast.
			if err := stream.Send(&agentpb.VideoFrame{
				Data:        frame.data,
				TimestampNs: frame.tsNs,
				Codec:       frame.codec,
				RawFormat:   frame.rawFmt,
			}); err != nil {
				return err
			}
		}
	}
}

// streamIPCamera bridges a network camera into the same hub fan-out used by local
// cameras, so subscriber accounting, teardown and the client-side keyframe buffer
// are all shared.
func (s *VideoService) streamIPCamera(stream grpc.ServerStreamingServer[agentpb.VideoFrame], src videoSource, req *agentpb.StreamVideoRequest) error {
	// Fail before creating a hub when the camera has no login or no known
	// address, so the caller gets the actionable error rather than a producer
	// that dies immediately afterwards.
	if err := s.preflightIPCamera(src.camera); err != nil {
		return err
	}

	// Counts this viewer toward loopback pump demand for the duration of the
	// stream (spec: a pump runs "when `camera view` attaches"), best-effort:
	// AcquireView's returned release func is always safe to call, even when
	// the v4l2loopback module is unavailable, so this never gates the direct
	// hub path streamed below.
	if s.loopback != nil {
		defer s.loopback.AcquireView(src.camera.ID)()
	}

	h, id, ch, err := s.getOrCreateHub(stream.Context(), src.key, req, hubHolderStreamClient)
	if err != nil {
		return err
	}
	defer h.unsubscribe(id)
	return s.pumpFrames(stream, h, id, ch)
}

// preflightIPCamera checks the conditions that would otherwise surface as a
// producer dying the instant it starts: no stored login, no known address, and a
// camera that cannot be reached at all.
//
// The reachability test matters more than it looks. A camera whose segment has
// lost its address is not merely absent: the request falls to the default route,
// is black-holed on the uplink, and RTSP spends twenty seconds timing out before
// reporting a generic pipeline failure. Testing the port first turns that into an
// immediate message naming the address and when the camera was last seen.
func (s *VideoService) preflightIPCamera(cam ipcam.Camera) error {
	cred, err := s.ipCameraCredentials(cam)
	if err != nil {
		return err
	}
	if _, err := ipcam.StreamURL(cam, cred, ipcam.StreamAuto); err != nil {
		return status.Errorf(codes.FailedPrecondition, "%s", ipcam.FormatUnreachable(cam))
	}
	if !s.cameraReachable(cam.Address) {
		return status.Errorf(codes.FailedPrecondition, "%s", ipcam.FormatUnreachable(cam))
	}
	return nil
}

// runIPProducer resolves the camera behind a hub key and streams it.
//
// The requested width selects the stream. That is safe despite a hub being
// shared, because getOrCreateHub already refuses a subscriber whose stream
// parameters differ from the running hub's.
func (s *VideoService) runIPProducer(ctx context.Context, broadcast func([]byte, frameTimestamp, agentpb.VideoCodec) bool, key string, req *agentpb.StreamVideoRequest) error {
	devID, err := strconv.ParseUint(strings.TrimPrefix(key, ipHubKeyPrefix), 10, 32)
	if err != nil {
		return status.Errorf(codes.Internal, "malformed camera key %q", key)
	}
	src, err := s.resolveSource(uint32(devID))
	if err != nil {
		return err
	}
	cred, err := s.ipCameraCredentials(src.camera)
	if err != nil {
		return err
	}
	streamURL, err := ipcam.StreamURL(src.camera, cred, ipcam.ChooseStream(req.GetWidth()))
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "%s", ipcam.FormatUnreachable(src.camera))
	}
	s.logger.Info("streaming network camera",
		zap.Uint32("id", uint32(devID)),
		zap.String("url", ipcam.RedactURL(streamURL)))
	return s.runGStreamer(ctx, ipcam.PipelineArgs(streamURL), func(chunk []byte) {
		broadcast(chunk, realtimeFrameTimestamp(time.Now()), agentpb.VideoCodec_VIDEO_CODEC_H264)
	})
}

// errCaptureSetup classifies a buffer-setup or streaming ioctl failure. Which ioctl refuses a
// second consumer is driver- and timing-dependent: S_FMT only refuses once the holder has
// allocated buffers. The errno is logged, not returned -- it would let a client probe the host.
func (s *VideoService) errCaptureSetup(ioctl, path string, errno unix.Errno) error {
	if isBusyErrno(errno) {
		return errCameraInUse(path)
	}
	s.logger.Error("V4L2 capture setup failed",
		zap.String("device", path), zap.String("ioctl", ioctl), zap.Error(errno))
	return status.Errorf(codes.Internal, "failed to start video capture")
}

// streamV4L2Native opens the V4L2 device, configures H.264 output via VIDIOC_S_FMT,
// allocates mmap buffers, and streams frames until ctx is cancelled or an error occurs.
// Each captured frame is delivered via the broadcast callback; if the callback returns
// false the loop exits cleanly (no subscribers remain).
// Returns nativeH264NotSupported if the device rejects the H.264 pixel format.
func (s *VideoService) streamV4L2Native(ctx context.Context, broadcast func([]byte, frameTimestamp, agentpb.VideoCodec) bool, path string, req *agentpb.StreamVideoRequest) error {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		s.logger.Error("failed to open video device", zap.String("device", path), zap.Error(err))
		if isBusyErrno(err) {
			return errCameraInUse(path)
		}
		return status.Errorf(codes.Internal, "failed to open video device")
	}
	defer unix.Close(fd) //nolint:errcheck

	// Configure H.264 output format.
	var vfmt v4l2Format
	vfmt.Type = v4l2BufTypeVideoCapture
	vfmt.Width = req.GetWidth()
	vfmt.Height = req.GetHeight()
	// "No preference" must not become "smallest mode the driver has" — see
	// bestDefaultFrameSize. Only fills in what the caller left unset, so an
	// explicit request is still honoured verbatim.
	if vfmt.Width == 0 || vfmt.Height == 0 {
		if w, h := bestDefaultFrameSize(fd, v4l2PixFmtH264); w > 0 && h > 0 {
			if vfmt.Width == 0 {
				vfmt.Width = w
			}
			if vfmt.Height == 0 {
				vfmt.Height = h
			}
			s.logger.Info("selected default capture size",
				zap.String("device", path), zap.Uint32("width", vfmt.Width),
				zap.Uint32("height", vfmt.Height))
		}
	}
	vfmt.PixelFormat = v4l2PixFmtH264
	vfmt.Field = v4l2FieldNone

	sfmt := func() unix.Errno {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocSFmt, uintptr(unsafe.Pointer(&vfmt)))
		return errno
	}
	if errno := retryWhileBusy(ctx, sfmt); errno != 0 {
		if errno == unix.EINVAL {
			return nativeH264NotSupported{msg: fmt.Sprintf("VIDIOC_S_FMT H264 rejected: %v", errno)}
		}
		s.logger.Error("VIDIOC_S_FMT failed", zap.String("device", path), zap.Error(errno))
		// Refuses only once the holder has allocated buffers; later sites cover the rest.
		if isBusyErrno(errno) {
			return errCameraInUse(path)
		}
		return status.Errorf(codes.Internal, "failed to configure video device")
	}
	if vfmt.PixelFormat != v4l2PixFmtH264 {
		return nativeH264NotSupported{msg: "device switched pixel format away from H264"}
	}

	// Best-effort: cap the camera encoder's keyframe interval. Non-fatal — many
	// UVC cameras reject this and keep their firmware default.
	s.setV4L2KeyframeInterval(fd, keyframeIntervalFrames(req.GetFramerate()))

	// Two buffers: one dequeued/in-flight, one queued for the camera to fill.
	// More buffers increase kernel-side lag when the broadcast lags the camera.
	const numBuffers = 2
	var req4 v4l2ReqBuffers
	req4.Count = numBuffers
	req4.Type = v4l2BufTypeVideoCapture
	req4.Memory = v4l2MemoryMmap

	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocReqbufs, uintptr(unsafe.Pointer(&req4))); errno != 0 {
		return s.errCaptureSetup("VIDIOC_REQBUFS", path, errno)
	}
	if req4.Count < 2 {
		return status.Errorf(codes.Internal, "insufficient buffer memory on device")
	}

	// Map and queue each buffer.
	mapped := make([][]byte, req4.Count)

	for i := uint32(0); i < req4.Count; i++ {
		var qbuf v4l2Buf
		qbuf.setIndex(i)
		qbuf.setType(v4l2BufTypeVideoCapture)
		qbuf.setMemory(v4l2MemoryMmap)

		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocQuerybuf, uintptr(unsafe.Pointer(&qbuf))); errno != 0 {
			return s.errCaptureSetup("VIDIOC_QUERYBUF", path, errno)
		}

		length := uint32(*(*uint32)(unsafe.Pointer(&qbuf[72]))) // length at offset 72 in v4l2_buffer
		data, err := unix.Mmap(fd, int64(qbuf.offset()), int(length), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return status.Errorf(codes.Internal, "mmap buffer %d: %v", i, err)
		}
		mapped[i] = data

		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocQbuf, uintptr(unsafe.Pointer(&qbuf))); errno != 0 {
			return s.errCaptureSetup("VIDIOC_QBUF", path, errno)
		}
	}
	defer func() {
		for _, data := range mapped {
			unix.Munmap(data) //nolint:errcheck
		}
	}()

	// Start streaming.
	bufType := uint32(v4l2BufTypeVideoCapture)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocStreamon, uintptr(unsafe.Pointer(&bufType))); errno != 0 {
		return s.errCaptureSetup("VIDIOC_STREAMON", path, errno)
	}
	defer func() {
		unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocStreamoff, uintptr(unsafe.Pointer(&bufType))) //nolint:errcheck
	}()

	if fd > math.MaxInt32 {
		return status.Errorf(codes.Internal, "file descriptor value out of range for poll")
	}
	pollFds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	var framesSent int
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Poll with a short timeout so context cancellation is noticed quickly.
		// VIDIOC_DQBUF blocks until a buffer arrives; without this a cancelled
		// context can wait up to one full frame period before the producer exits,
		// holding the device fd and delaying the next StreamVideo caller.
		ready, err := unix.Poll(pollFds, 100)
		if err == unix.EINTR || (err == nil && ready == 0) {
			continue // timeout or signal — re-check ctx.Done
		}
		if err != nil {
			s.logger.Error("poll failed on video device", zap.String("device", path), zap.Error(err))
			return status.Errorf(codes.Internal, "video device poll error")
		}

		var dqbuf v4l2Buf
		dqbuf.setType(v4l2BufTypeVideoCapture)
		dqbuf.setMemory(v4l2MemoryMmap)

		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocDqbuf, uintptr(unsafe.Pointer(&dqbuf))); errno != 0 {
			if errno == unix.EINTR || errno == unix.EAGAIN {
				continue
			}
			// Before the H264 fallback below: EBUSY is a held camera, not a device that
			// cannot encode, so it must reach the sharing path, not the software encoder.
			if isBusyErrno(errno) {
				return errCameraInUse(path)
			}
			// Device accepted H264 format but failed before delivering any frame —
			// signal the caller to fall back to the GStreamer software encoder.
			if framesSent == 0 {
				return nativeH264NotSupported{msg: fmt.Sprintf("VIDIOC_DQBUF failed before first frame: %v", errno)}
			}
			return s.errCaptureSetup("VIDIOC_DQBUF", path, errno)
		}

		idx := dqbuf.index()
		if n := dqbuf.bytesUsed(); n > 0 {
			// Cap at maxFrameBytes before allocating: a misbehaving or compromised
			// V4L2 driver could report bytesUsed up to the full mmap region size.
			// Capping here bounds the allocation at the source rather than relying
			// solely on the drop check inside broadcast().
			if n > maxFrameBytes {
				n = maxFrameBytes
			}
			// Copy out of the mmap region before requeuing: the slice handed to
			// subscribers must not alias a buffer the camera may refill.
			data := make([]byte, n)
			copy(data, mapped[idx][:n])
			stamp := realtimeFrameTimestamp(time.Now())
			stamp.nativeNs = dqbuf.timestampNanos()
			stamp.nativeFlags = dqbuf.flags()
			stamp.sequence = dqbuf.sequence()
			stamp.sequenceValid = true
			// V4L2 compressed capture delivers exactly one encoded frame per
			// dequeued buffer, so this is the one producer whose frames are
			// whole access units. The maxFrameBytes cap below can truncate a
			// pathologically large frame, in which case the alignment promise
			// no longer holds.
			stamp.auAligned = n == dqbuf.bytesUsed()
			const v4l2TimestampMask = uint32(0x0000e000)
			const v4l2TimestampMonotonic = uint32(0x00002000)
			if stamp.nativeFlags&v4l2TimestampMask == v4l2TimestampMonotonic {
				stamp.nativeClock = "CLOCK_MONOTONIC_V4L2"
			} else {
				stamp.nativeClock = "V4L2_TIMESTAMP_UNKNOWN"
			}
			if !broadcast(data, stamp, agentpb.VideoCodec_VIDEO_CODEC_H264) {
				return nil
			}
			framesSent++
		}

		// Re-queue the buffer.
		var qbuf v4l2Buf
		qbuf.setIndex(idx)
		qbuf.setType(v4l2BufTypeVideoCapture)
		qbuf.setMemory(v4l2MemoryMmap)
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocQbuf, uintptr(unsafe.Pointer(&qbuf))); errno != 0 {
			return s.errCaptureSetup("VIDIOC_QBUF", path, errno)
		}
	}
}

// setV4L2KeyframeInterval caps the camera encoder's keyframe interval to gop
// frames so a dropped frame self-heals quickly and a client can resync within
// ~0.5s. It is best-effort: many UVC cameras do not expose these controls and
// reject them with EINVAL — that is logged and ignored, leaving the firmware
// default in place. The H.264-specific I-period control is tried first, then
// the generic MPEG GOP-size control.
func (s *VideoService) setV4L2KeyframeInterval(fd, gop int) {
	for _, ctl := range []struct {
		name string
		id   uint32
	}{
		{"V4L2_CID_MPEG_VIDEO_H264_I_PERIOD", v4l2CIDH264IPeriod},
		{"V4L2_CID_MPEG_VIDEO_GOP_SIZE", v4l2CIDGOPSize},
	} {
		errno := setV4L2ExtControl(fd, ctl.id, int32(gop), v4l2CtrlClassCodec)
		if errno == 0 {
			s.logger.Info("V4L2 keyframe interval set",
				zap.String("control", ctl.name), zap.Int("frames", gop))
			return
		}
		s.logger.Debug("V4L2 keyframe control rejected, trying next",
			zap.String("control", ctl.name), zap.String("errno", errno.Error()))
	}
	s.logger.Info("V4L2 keyframe interval not configurable; using camera default",
		zap.Int("requested_frames", gop))
}

// setV4L2ExtControl issues VIDIOC_S_EXT_CTRLS to set a single integer control,
// returning the raw errno (0 on success). The inner v4l2_ext_control is reached
// only through a uintptr stored inside the outer struct, where the garbage
// collector cannot see it, so it is pinned for the duration of the syscall.
func setV4L2ExtControl(fd int, controlID uint32, value int32, which uint32) unix.Errno {
	var ctrl v4l2ExtControl
	ctrl.setID(controlID)
	ctrl.setValue(value)

	var pinner runtime.Pinner
	pinner.Pin(&ctrl)
	defer pinner.Unpin()

	var ctrls v4l2ExtControls
	ctrls.setWhich(which)
	ctrls.setCount(1)
	ctrls.setControlsPtr(uintptr(unsafe.Pointer(&ctrl)))

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocSExtCtrls, uintptr(unsafe.Pointer(&ctrls)))
	return errno
}

// limitedBuffer is a bytes.Buffer wrapper that silently drops writes beyond limit
// bytes. Used for GStreamer stderr so a misbehaving or crashing process cannot
// exhaust the heap via unbounded stderr output.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

// maxStderrBytes caps how much subprocess stderr is retained for diagnostics.
const maxStderrBytes = 64 * 1024

func (l *limitedBuffer) Write(p []byte) (int, error) {
	remaining := l.limit - l.buf.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	return l.buf.Write(p)
}

// gstFallbackDirs is the list of directories searched for GStreamer binaries
// when they are not on PATH. wendy-agent runs as a systemd service whose
// inherited PATH may omit the standard system bin directories (observed on
// wendyOS, where a CUDA setup file leaves PATH=/usr/local/cuda-XX/bin:$PATH
// — the literal "$PATH" not being expanded). Declared as a var so tests can
// override it.
var gstFallbackDirs = []string{"/usr/bin", "/usr/local/bin", "/usr/sbin"}

// resolveGSTBinary looks up a GStreamer binary on PATH first, then falls back
// to known system locations.
func resolveGSTBinary(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	for _, dir := range gstFallbackDirs {
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found; install GStreamer on the device", name)
}

// streamGStreamer spawns gst-launch-1.0 on the device to encode via the best available
// encoder and pipes the resulting stream back as videoFrame chunks via the broadcast callback.
func (s *VideoService) streamGStreamer(ctx context.Context, broadcast func([]byte, frameTimestamp, agentpb.VideoCodec) bool, path string, req *agentpb.StreamVideoRequest, transport camera.Transport, libcameraID string, pw pipeWireSource, sink rawSink) (runErr error) {
	if sink == nil {
		sink = noRawSink{}
	}
	gstPath, err := resolveGSTBinary("gst-launch-1.0")
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	inspectPath, err := resolveGSTBinary("gst-inspect-1.0")
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	}

	// The element set decides both the encoder and the CSI capture source
	// (libcamerasrc / nvarguscamerasrc), so list once and reuse it.
	available, listErr := listGSTElements(inspectPath)
	if listErr != nil {
		// findGStreamerEncoderFromSet handles a nil set by attempting x264enc.
		s.logger.Debug("gst-inspect listing failed", zap.Error(listErr))
		available = nil
	}

	if pw.serial != 0 {
		if listErr != nil {
			// A listing failure is not contention; saying so would send the operator
			// after a process that does not hold the camera.
			return status.Errorf(codes.Internal, "cannot check for gstreamer1.0-pipewire: %v", listErr)
		}
		// Name the missing package rather than silently opening the device this
		// call was chosen to avoid: pipewiresrc ships separately from the daemon.
		if !available["pipewiresrc"] {
			s.logger.Warn("gstreamer1.0-pipewire is missing; cannot read a camera another client holds",
				zap.String("device", path))
			return errCameraInUse(path)
		}
		// Probe here, not at the call site: the element set is what says whether a
		// jpegdec stage can be built at all, and an absent one must not reach the
		// pipeline. Also keeps the 2s probe off the path that rejects pipewiresrc.
		if available["jpegdec"] {
			jpeg, known := s.nodeServesJPEG(ctx, gstPath, pw.serial)
			pw.jpeg = jpeg
			if !known {
				// Treated as raw. A wrong guess costs the first-frame deadline below,
				// after which the caller reports the contention that sent us here.
				s.logger.Info("PipeWire node format unknown; assuming raw",
					zap.String("device", path), zap.Uint64("node_serial", pw.serial))
			}
		}
	}

	enc, err := findGStreamerEncoderFromSet(available)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	if !isValidGSTElementName(enc.element) {
		return status.Errorf(codes.Internal, "GStreamer encoder name contains invalid characters")
	}
	// Explicitly validate the device path before interpolating into the pipeline
	// string. path is always fmt.Sprintf("/dev/video%d", devID) where devID is a
	// range-validated uint32, so it will never contain GStreamer pipeline tokens,
	// but this check makes the invariant auditable in the diff.
	if !isValidGSTDevicePath(path) {
		return status.Errorf(codes.Internal, "unexpected device path format")
	}
	s.logger.Info("GStreamer encoder selected", zap.String("encoder", enc.element), zap.String("codec", enc.codec.String()))

	var plan gstPipelinePlan
	if useArgusSource(transport, s.hostIsJetson(), available) {
		// Argus indexes sensors by sensor-id; /dev/videoN maps to sensor-id N for
		// the common single-CSI-camera case. The device id was already range-checked
		// (<= maxVideoDeviceID) and Lstat-gated in StreamVideo, and camera access is
		// authorized at the entitlement level, so there is no per-camera authorization
		// here for a crafted id to bypass.
		sensorID := int(req.GetDeviceId())
		s.logger.Info("CSI camera on Jetson — capturing via nvarguscamerasrc (Argus)",
			zap.Int("sensor_id", sensorID), zap.String("encoder", enc.element))
		plan.args = buildArgusGStreamerArgs(gstPath, req, sensorID, enc.element, enc.hasH264Parse, available)
		plan.rawWhy = "CSI cameras on Jetson are captured through Argus; raw frames are not offered"
	} else {
		plan, err = planGStreamerPipeline(gstPath, path, req, enc.element, enc.hasH264Parse, transport, libcameraID, pw, available)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to build GStreamer pipeline: %v", err)
		}
	}
	args := plan.args
	var cmd *exec.Cmd
	if pw.serial != 0 {
		// The agent runs as root outside any session, so a PipeWire client it
		// spawns must be pointed at the wendy session. Without it the client
		// reaches the system-wide daemon, whose graph is empty.
		var envErr error
		if cmd, envErr = audio.Command(ctx, args[0], args[1:]...); envErr != nil {
			return status.Errorf(codes.Unavailable, "PipeWire session unavailable: %v", envErr)
		}
	} else {
		cmd = exec.CommandContext(ctx, args[0], args[1:]...)
	}
	// The busy classifier reads gst's prose. LC_ALL=C is the one value glibc short-circuits
	// ahead of LANGUAGE, so messages cannot come back translated; exec keeps the last entry.
	cmd.Env = append(cmd.Environ(), "LC_ALL=C")
	stderrBuf := &limitedBuffer{limit: maxStderrBytes}
	cmd.Stderr = stderrBuf

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create GStreamer pipe: %v", err)
	}
	// The raw tap is a second pipe the pipeline's tee writes to (child fd 3, the
	// first ExtraFiles slot). It is created before Start so the child inherits it,
	// and the parent's write end is closed right after, so EOF reaches the reader
	// when the child exits.
	var rawR, rawW *os.File
	if plan.raw != nil {
		rawR, rawW, err = os.Pipe()
		if err != nil {
			return status.Errorf(codes.Internal, "failed to create raw frame pipe: %v", err)
		}
		cmd.ExtraFiles = []*os.File{rawW}
	}
	if err := cmd.Start(); err != nil {
		if rawR != nil {
			rawR.Close() //nolint:errcheck
			rawW.Close() //nolint:errcheck
		}
		return status.Errorf(codes.Internal, "failed to start GStreamer: %v", err)
	}
	if plan.raw != nil {
		rawW.Close() //nolint:errcheck
		sink.rawOffered(plan.raw)
		s.logger.Info("raw frame tap open", zap.String("device", path),
			zap.String("format", describeRawFormat(plan.raw)))
		go s.pumpRawTap(ctx, rawR, plan.raw, sink, path)
	} else {
		sink.rawNotOffered(plan.rawWhy)
	}

	defer func() {
		cmd.Process.Kill() //nolint:errcheck
		if rawR != nil {
			rawR.Close() //nolint:errcheck // unblocks pumpRawTap
		}
		io.Copy(io.Discard, stdout) // drain so Wait's internal goroutine can exit
		waitErr := cmd.Wait()
		if runErr == nil {
			// Log stderr internally — do NOT embed in the gRPC response. GStreamer
			// stderr routinely includes device paths, kernel module names, library
			// versions, and pipeline topology. Returning it verbatim lets an
			// authenticated client enumerate the system via deliberate failures.
			msg := strings.TrimSpace(stderrBuf.buf.String())
			if msg != "" {
				s.logger.Error("GStreamer pipeline failed", zap.String("device", path), zap.String("stderr", msg))
				// Contention is the one cause an operator can act on, and it is only ever
				// reported as prose on stderr. Gated on the pipeline failing by itself: gst
				// also prints busy warnings while probing modes it then recovers from.
				if exitedOnError(waitErr) && isBusyStderr(msg, path) {
					runErr = errCameraInUse(path)
				} else {
					runErr = status.Errorf(codes.Internal, "GStreamer pipeline failed; see agent logs for details")
				}
			} else if waitErr != nil {
				runErr = status.Errorf(codes.Internal, "GStreamer pipeline failed; see agent logs for details")
			}
		}
	}()

	const chunkSize = 256 * 1024
	buf := make([]byte, chunkSize)

	// A mismatched pipeline stalls rather than fails: gst stays alive producing nothing and
	// stdout.Read blocks on a pipe with no deadline, so `camera view` hung with no frame and no
	// error. Kept under the dashboard player's 25s budget so this error wins that race.
	var timedOut atomic.Bool
	firstChunk := time.AfterFunc(firstFrameTimeout, func() {
		timedOut.Store(true)
		if cmd.Process != nil {
			cmd.Process.Kill() //nolint:errcheck
		}
	})
	defer firstChunk.Stop()

	// gstMaxFrameRate is the maximum number of frame allocations per second from
	// the GStreamer read loop. A misbehaving or adversarially replaced gst-launch
	// binary could write at arbitrarily high rate; bounding the allocation rate
	// prevents it from forcing excessive GC pressure. Chunks arriving faster than
	// this are discarded — H.264/VP8 byte streams self-synchronise at I-frames.
	const gstMaxFrameRate = 240
	minFrameInterval := time.Second / gstMaxFrameRate
	var lastFrameTime time.Time

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, readErr := stdout.Read(buf)
		if n > 0 {
			firstChunk.Stop()
			now := time.Now()
			passFrame := lastFrameTime.IsZero() || now.Sub(lastFrameTime) >= minFrameInterval
			lastFrameTime = now // always update to prevent burst bypass after a gap
			if passFrame {
				if n > maxFrameBytes {
					n = maxFrameBytes
				}
				data := make([]byte, n)
				copy(data, buf[:n])
				if !broadcast(data, realtimeFrameTimestamp(now), enc.codec) {
					return nil
				}
			}
		}
		if readErr != nil {
			if timedOut.Load() && lastFrameTime.IsZero() {
				return status.Errorf(codes.DeadlineExceeded,
					"camera produced no video within %s", firstFrameTimeout)
			}
			if readErr == io.EOF {
				return nil // normal termination; defer surfaces stderr/exit errors
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return status.Errorf(codes.Internal, "failed to read GStreamer output: %v", readErr)
		}
	}
}

// gstEncoderResult describes a found GStreamer encoder and the codec it produces.
type gstEncoderResult struct {
	element      string
	codec        agentpb.VideoCodec
	hasH264Parse bool // whether h264parse is available on this device
}

// findGStreamerEncoder probes available encoders by listing all elements once via
// gst-inspect-1.0 (no args) and building a lookup set. Per-element subprocess calls
// are unreliable on some builds; the list command is authoritative.
func findGStreamerEncoder(inspectPath string) (gstEncoderResult, error) {
	available, err := listGSTElements(inspectPath)
	if err != nil {
		// If listing fails, attempt x264enc and let gst-launch fail with a clear message.
		return gstEncoderResult{element: "x264enc", codec: agentpb.VideoCodec_VIDEO_CODEC_H264}, nil
	}
	return findGStreamerEncoderFromSet(available)
}

// Encoders whose element registers even when the hardware is absent: Thor has no NVENC and
// ships /dev/v4l2-nvenc as a /dev/null stub, so it loads and then fails to open.
var encoderDeviceNodes = map[string]string{
	"nvv4l2h264enc": "/dev/v4l2-nvenc",
}

// A stub such as /dev/null opens fine but rejects VIDIOC_QUERYCAP with ENOTTY. O_RDWR matches
// how the encoder element opens it; no O_NOFOLLOW, since a platform may symlink the node and
// the paths above are hardcoded. Indirected for tests.
var v4l2NodeUsable = func(path string) bool {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd) //nolint:errcheck
	var caps v4l2Capability
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocQueryCap, uintptr(unsafe.Pointer(&caps)))
	return errno == 0
}

// findGStreamerEncoderFromSet performs encoder selection against a precomputed
// element availability map. When available is nil (e.g. gst-inspect listing
// failed), it falls back to attempting x264enc.
func findGStreamerEncoderFromSet(available map[string]bool) (gstEncoderResult, error) {
	if available == nil {
		return gstEncoderResult{element: "x264enc", codec: agentpb.VideoCodec_VIDEO_CODEC_H264}, nil
	}

	// An element needing a V4L2 node counts as available only if that node really works.
	// Probed at most once per element: hasElem is consulted from several fallback paths, and
	// reopening a real encoder node each time is wasteful.
	probed := make(map[string]bool)
	hasElem := func(name string) bool {
		if !available[name] {
			return false
		}
		node, gated := encoderDeviceNodes[name]
		if !gated {
			return true
		}
		if usable, seen := probed[name]; seen {
			return usable
		}
		usable := v4l2NodeUsable(node)
		probed[name] = usable
		return usable
	}
	h264Parse := hasElem("h264parse")

	h264Encoders := []string{
		"nvv4l2h264enc", // NVIDIA V4L2 hardware (Jetson L4T, gstreamer1.0-plugins-nvvideo4linux2)
		"v4l2h264enc",   // V4L2 M2M hardware (gst-plugins-good)
		"omxh264enc",    // OpenMAX hardware (Broadcom, Qualcomm)
		"avenc_h264",    // libavcodec bridge (gst-libav)
		"x264enc",       // software (gst-plugins-ugly)
		"openh264enc",   // software (gst-plugins-bad)
		"vaapih264enc",  // Intel VA-API
		"nvh264enc",     // NVIDIA NVENC (desktop)
		"msdkh264enc",   // Intel Media SDK
	}

	// H.264 is preferred when h264parse is available to normalize output to Annex B
	// byte-stream. Without h264parse, encoders like x264enc emit stream-format=avc
	// which discards SPS/PPS when piped raw over gRPC, making the stream undecodable.
	if h264Parse {
		for _, enc := range h264Encoders {
			if hasElem(enc) {
				return gstEncoderResult{element: enc, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, hasH264Parse: true}, nil
			}
		}
		for name := range available {
			lower := strings.ToLower(name)
			if strings.Contains(lower, "h264") && strings.Contains(lower, "enc") && hasElem(name) {
				return gstEncoderResult{element: name, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, hasH264Parse: true}, nil
			}
		}
	}

	// VP8 preferred over raw H.264 when h264parse is absent: vp8enc+webmmux (both
	// in gst-plugins-good) produce a self-describing WebM container that requires no
	// stream-format negotiation and is always decodable by the client.
	if hasElem("vp8enc") && hasElem("webmmux") {
		return gstEncoderResult{element: "vp8enc", codec: agentpb.VideoCodec_VIDEO_CODEC_VP8}, nil
	}

	// Last resort: attempt H.264 without normalization. Hardware encoders such as
	// nvv4l2h264enc and v4l2h264enc typically emit byte-stream natively; x264enc may
	// produce AVC which the client's h264parse may or may not be able to decode.
	for _, enc := range h264Encoders {
		if hasElem(enc) {
			return gstEncoderResult{element: enc, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, hasH264Parse: false}, nil
		}
	}
	for name := range available {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "h264") && strings.Contains(lower, "enc") && hasElem(name) {
			return gstEncoderResult{element: name, codec: agentpb.VideoCodec_VIDEO_CODEC_H264}, nil
		}
	}

	return gstEncoderResult{}, fmt.Errorf(
		"no supported GStreamer encoder found (checked %d elements); install gst-plugins-good (vp8enc+webmmux) or gst-plugins-bad (h264parse)+gst-plugins-ugly (x264enc)",
		len(available),
	)
}

// listGSTElements runs gst-inspect-1.0 once and returns a set of all available element names.
// Each output line has the form "plugin:  element: description".
func listGSTElements(inspectPath string) (map[string]bool, error) {
	// Bounded: this walks the whole plugin registry on a path an operator is already waiting on.
	ctx, cancel := context.WithTimeout(context.Background(), gstInspectTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, inspectPath).Output()
	if err != nil {
		return nil, fmt.Errorf("gst-inspect-1.0: %w", err)
	}
	elements := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		// Split on ": " to get plugin and element name (first two fields).
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 2 {
			continue
		}
		name := strings.TrimSpace(parts[1])
		if name != "" && !strings.ContainsAny(name, " \t") {
			elements[name] = true
		}
	}
	return elements, nil
}

func keyframeIntervalFrames(fps uint32) int {
	if fps == 0 {
		fps = 30
	}
	gop := int(fps) / 2
	if gop < 1 {
		gop = 1
	}
	return gop
}

// leakyRawQueue is a GStreamer queue placed between the V4L2 source and the
// encoder. The agent reads a continuous encoded byte stream from the encoder, so
// arbitrary encoded bytes cannot be dropped; instead this queue drops *raw*
// frames when capture outruns the encoder/gRPC send. leaky=downstream evicts the
// oldest buffered frame, so the encoder always works on the freshest raw frame
// and a capture backlog drains by skipping rather than encoding stale frames.
// Dropping raw input never desyncs the encoded GOP. max-size-bytes/-time are
// disabled so only the 2-buffer count bounds the queue.
const leakyRawQueue = "queue max-size-buffers=2 max-size-bytes=0 max-size-time=0 leaky=downstream"

// gstPipelinePlan is what planGStreamerPipeline decided: the gst-launch argument
// list, and whether the pipeline tees raw capture frames to rawTapFD.
type gstPipelinePlan struct {
	args []string
	// raw describes the frames written to rawTapFD; nil when the pipeline offers none.
	raw *agentpb.RawFormat
	// rawWhy says why raw is not offered — the reason a raw subscriber is refused with.
	rawWhy string
}

// buildGStreamerArgs constructs the gst-launch-1.0 argument list for V4L2 encode.
// Returns an error if any interpolated string contains GStreamer pipeline injection
// tokens — making the security property a hard failure at construction time rather
// than relying solely on caller-side allowlist validation.
func buildGStreamerArgs(gstPath, devicePath string, req *agentpb.StreamVideoRequest, encoder string, hasH264Parse bool, transport camera.Transport, libcameraID string, pw pipeWireSource, available map[string]bool) ([]string, error) {
	plan, err := planGStreamerPipeline(gstPath, devicePath, req, encoder, hasH264Parse, transport, libcameraID, pw, available)
	if err != nil {
		return nil, err
	}
	return plan.args, nil
}

// planGStreamerPipeline is buildGStreamerArgs plus the raw-tap decision: whether
// this pipeline can also hand out the camera's uncompressed frames, and if not,
// why (see video_raw_tap.go for the policy).
func planGStreamerPipeline(gstPath, devicePath string, req *agentpb.StreamVideoRequest, encoder string, hasH264Parse bool, transport camera.Transport, libcameraID string, pw pipeWireSource, available map[string]bool) (gstPipelinePlan, error) {
	var plan gstPipelinePlan
	// Validate numeric request parameters here (not only at StreamVideo entry) so
	// buildGStreamerArgs is safe regardless of call site — prevents injection via
	// unbounded width/height/framerate values if called from a different path.
	if err := validateStreamParams(devicePath, req); err != nil {
		return plan, fmt.Errorf("invalid stream parameters for GStreamer pipeline: %w", err)
	}
	for _, s := range []string{devicePath, encoder} {
		// Space and tab are included because buildGStreamerArgs splits the pipeline
		// string with strings.Fields — a space in a validated value would inject
		// extra tokens into the argument list even if pipeline operators are blocked.
		if strings.ContainsAny(s, "!(); \t") {
			return plan, fmt.Errorf("GStreamer argument contains pipeline injection token: %q", s)
		}
	}
	// For CSI cameras the source is libcamerasrc (with a validated camera-name);
	// otherwise v4l2src on the device path. libcameraID is validated inside
	// buildSourceElement, so it is not subject to the devicePath/encoder check above.
	src := buildSourceElement(devicePath, transport, libcameraID, pw, available)
	usingPipeWire := strings.HasPrefix(src, "pipewiresrc")
	// GOP is in frames. On the PipeWire path videorate only caps the rate, so a
	// graph running slower than requested spaces keyframes further apart in time.
	gop := keyframeIntervalFrames(req.GetFramerate())

	// libcamerasrc (CSI/PiSP) must be pinned to a processed format or it
	// negotiates raw Bayer (e.g. the Raspberry Pi 5 rp1-cfe/PiSP pipeline), which
	// no downstream videoconvert/encoder can consume — the camera reports
	// Camera::configure() -22 and the pipeline dies with not-negotiated. NV12 is
	// the PiSP ISP's native output. A USB v4l2src keeps negotiating its own native
	// format (YUYV/MJPEG/...). Any requested dimensions are folded into this same
	// source capsfilter; a formatless width/height filter still lets libcamerasrc
	// fall back to Bayer.
	var capsParts []string
	if strings.HasPrefix(src, "libcamerasrc") {
		capsParts = append(capsParts, "format=NV12")
	}
	// No dimension may be pinned on pipewiresrc: once another client is streaming, that
	// trips "assertion 'gst_caps_is_fixed' failed". Naming the media type alone is fine.
	// Convert after the queue instead, where videoscale and videorate take what they get.
	var capW, capH uint32
	var convertStages []string
	if usingPipeWire {
		// Rate first: the graph serves whatever the first client negotiated, so
		// scaling ahead of videorate pays for frames it is about to drop.
		if req.GetFramerate() > 0 {
			convertStages = append(convertStages, fmt.Sprintf("videorate max-rate=%d", req.GetFramerate()))
		}
		if req.GetWidth() > 0 && req.GetHeight() > 0 {
			convertStages = append(convertStages, "videoscale",
				fmt.Sprintf("video/x-raw,width=%d,height=%d", req.GetWidth(), req.GetHeight()))
		}
	} else {
		// "Default" must not mean "smallest": with no width/height the source
		// negotiates its first mode, which on UVC is the smallest.
		capW, capH = req.GetWidth(), req.GetHeight()
		if (capW == 0 || capH == 0) && !strings.HasPrefix(src, "libcamerasrc") {
			if bw, bh := bestDefaultFrameSizeForDevice(devicePath); bw > 0 && bh > 0 {
				if capW == 0 {
					capW = bw
				}
				if capH == 0 {
					capH = bh
				}
				// A stacked thermal module's "device default" is its stacked mode —
				// the picture plus the metadata rows — as long as videocrop can hide
				// those rows from viewers. Then one capture serves both audiences: the
				// companion app sees exactly the picture it saw before, and a raw
				// subscriber joining at the default gets the temperature rows too,
				// instead of being refused because a viewer already pinned the plain
				// mode. Only for a true default: a caller naming a size gets that size.
				if req.GetWidth() == 0 && req.GetHeight() == 0 && available["videocrop"] {
					if sw, sh := stackedModeAbove(devicePath, capW, capH); sh > 0 {
						capW, capH = sw, sh
					}
				}
			}
		}
		if capW > 0 {
			capsParts = append(capsParts, fmt.Sprintf("width=%d", capW))
		}
		if capH > 0 {
			capsParts = append(capsParts, fmt.Sprintf("height=%d", capH))
		}
		if req.GetFramerate() > 0 {
			capsParts = append(capsParts, fmt.Sprintf("framerate=%d/1", req.GetFramerate()))
		}
	}

	// Prefer compressed (MJPEG) capture from USB cameras when the device offers
	// it at the chosen size and jpegdec is available to unpack it. Raw capture
	// is bottlenecked by USB bandwidth, and UVC descriptors cap raw modes at
	// frame rates as low as 5 fps for 720p/1080p — the SAME camera delivers
	// 30 fps over MJPEG (measured on a Brio 101: 5 fps raw at both 1080p and
	// 720p). jpegdec is cheap relative to the H.264 encode that follows. The
	// leaky queue sits on the compressed side so backlog is dropped before,
	// not after, the decode work is spent.
	//
	// The PipeWire path negotiates whatever the graph serves, so it selects no
	// capture format of its own.
	useMJPEG := !strings.HasPrefix(src, "libcamerasrc") && !usingPipeWire &&
		capW > 0 && capH > 0 &&
		available["jpegdec"] &&
		deviceSupportsMJPEGSize(devicePath, capW, capH)

	// Raw tap: only a v4l2src pipeline that already captures raw video has frames
	// worth promising (see video_raw_tap.go). The format is pinned to what the tap
	// reports, so the bytes on fd 3 are exactly what RawFormat says they are.
	rawCapture := !strings.HasPrefix(src, "libcamerasrc") && !usingPipeWire && !useMJPEG
	switch {
	case strings.HasPrefix(src, "libcamerasrc"):
		plan.rawWhy = "CSI cameras are captured through the ISP pipeline; raw frames are not offered"
	case usingPipeWire:
		plan.rawWhy = "camera is shared through PipeWire; raw frames are not offered"
	case useMJPEG:
		plan.rawWhy = fmt.Sprintf("camera is captured as MJPEG at %dx%d; raw frames are not offered", capW, capH)
	case capW == 0 || capH == 0:
		plan.rawWhy = "camera advertises no discrete frame size to capture raw frames at"
	case rawFormatFor(devicePath, capW, capH) == nil:
		plan.rawWhy = fmt.Sprintf("camera advertises none of %s at %dx%d; raw frames are not offered",
			rawFormatNames(), capW, capH)
	case uint64(capH)*uint64(rawFormatFor(devicePath, capW, capH).bytesPerLine(capW)) > maxRawFrameBytes:
		f := rawFormatFor(devicePath, capW, capH)
		plan.rawWhy = fmt.Sprintf("a %dx%d %s frame exceeds the %d-byte raw frame limit",
			capW, capH, strings.TrimSpace(f.fourcc), maxRawFrameBytes)
	default:
		f := rawFormatFor(devicePath, capW, capH)
		plan.raw = &agentpb.RawFormat{
			Width:        capW,
			Height:       capH,
			Fourcc:       f.fourcc,
			BytesPerLine: f.bytesPerLine(capW),
		}
		capsParts = append(capsParts, "format="+f.gstFormat)
	}

	// The leaky queue goes as early as the source allows, so a backlog is shed
	// before any decode or scale work is spent on it. pw.jpeg only describes a
	// graph node, so it may not steer a pipeline that ended up on another source.
	stages := []string{src}
	switch {
	case useMJPEG:
		stages = append(stages, "image/jpeg,"+strings.Join(capsParts, ","), leakyRawQueue, "jpegdec")
	case usingPipeWire && pw.jpeg:
		// Name the type and decode it: videoconvert cannot take compressed input.
		stages = append(stages, "image/jpeg", leakyRawQueue, "jpegdec")
	default:
		if len(capsParts) > 0 {
			stages = append(stages, "video/x-raw,"+strings.Join(capsParts, ","))
		}
		if plan.raw != nil {
			// Branch the untouched capture off BEFORE the queue, crop and encoder,
			// so the raw subscriber sees the camera's frame and the viewer's path
			// is unchanged from here on. Each branch gets its own leaky queue: a
			// tee blocks on its slowest branch, and a viewer's picture must never
			// depend on how fast an analytic reader drains fd 3.
			stages = append(stages, "tee name="+rawTeeName)
		}
		stages = append(stages, leakyRawQueue)
	}
	// A stacked thermal mode carries metadata rows a viewer must not see (they
	// render as a stripe of noise). Encoded subscribers get the picture rows; the
	// raw tap, branched off above, keeps the whole frame for analytic readers.
	if rawCapture && capW > 0 && capH > 0 && available["videocrop"] {
		if rows := stackedMetadataRows(devicePath, capW, capH); rows > 0 {
			edge := "bottom"
			if thermalMetadataRowsOnTop {
				edge = "top"
			}
			stages = append(stages, fmt.Sprintf("videocrop %s=%d", edge, rows))
		}
	}
	stages = append(stages, convertStages...)
	stages = append(stages, encoderSegment(encoder, hasH264Parse, gop), "fdsink fd=1")
	pipeline := strings.Join(stages, " ! ")
	if plan.raw != nil {
		pipeline += fmt.Sprintf(" %s. ! %s ! fdsink fd=%d", rawTeeName, rawTapQueue, rawTapFD)
	}
	// -q suppresses gst-launch's status messages (e.g. "Setting pipeline to PLAYING")
	// from being written to stdout and corrupting the binary H264 stream.
	plan.args = append([]string{gstPath, "-q"}, strings.Fields(pipeline)...)
	return plan, nil
}

// transportToProto maps the internal camera.Transport to the proto enum.
func transportToProto(t camera.Transport) agentpb.VideoTransport {
	switch t {
	case camera.TransportUSB:
		return agentpb.VideoTransport_VIDEO_TRANSPORT_USB
	case camera.TransportCSI:
		return agentpb.VideoTransport_VIDEO_TRANSPORT_CSI
	case camera.TransportIP:
		return agentpb.VideoTransport_VIDEO_TRANSPORT_IP
	default:
		return agentpb.VideoTransport_VIDEO_TRANSPORT_UNKNOWN
	}
}

// hostIsJetson reports whether the agent host is an NVIDIA Jetson (selecting the
// Argus capture path for CSI cameras). nil-safe for tests that omit the seam.
func (s *VideoService) hostIsJetson() bool {
	if s.isJetson == nil {
		return false
	}
	return s.isJetson()
}

// lookupLibcameraID returns the libcamera camera-name to pass to libcamerasrc,
// but only for a CSI device and only when exactly one libcamera camera is
// enumerated (an unambiguous mapping). Returns "" otherwise; callers let
// libcamerasrc auto-select in that case.
func (s *VideoService) lookupLibcameraID(ctx context.Context, transport camera.Transport) string {
	if transport != camera.TransportCSI {
		return ""
	}
	ids, err := s.enumerateLibcamera(ctx)
	if err != nil || len(ids) != 1 {
		return ""
	}
	for id := range ids {
		return id
	}
	return ""
}

// buildSourceElement chooses the capture source element for the GStreamer
// pipeline:
//
//   - CSI with libcamerasrc available → "libcamerasrc [camera-name=<id>]"
//   - CSI without libcamerasrc        → "v4l2src device=<path>" (degraded)
//   - USB / Unknown                   → "v4l2src device=<path>"
//
// libcameraID originates from `cam --list` output and is the one externally
// sourced string interpolated into the pipeline, which is later split with
// strings.Fields — so it is validated here as a defense-in-depth check at the
// injection sink. An ID that fails validation is dropped and libcamerasrc
// auto-selects instead. (devicePath is always "/dev/video%d" formatted from a
// uint32 device id, so it cannot contain whitespace or pipeline separators.)
func buildSourceElement(devicePath string, transport camera.Transport, libcameraID string, pw pipeWireSource, available map[string]bool) string {
	if transport == camera.TransportCSI && available != nil && available["libcamerasrc"] {
		if camera.IsValidLibcameraID(libcameraID) {
			return fmt.Sprintf("libcamerasrc camera-name=%s", libcameraID)
		}
		return "libcamerasrc"
	}
	// pipewiresrc reads the camera through the graph, so the device node stays free for
	// other consumers. streamGStreamer checks the element ships before setting pw.serial.
	if pw.serial != 0 {
		return fmt.Sprintf("pipewiresrc target-object=%d", pw.serial)
	}
	return fmt.Sprintf("v4l2src device=%s", devicePath)
}

// useArgusSource reports whether the NVIDIA Argus capture path should be used:
// a CSI sensor, on a Jetson host, with the nvarguscamerasrc element installed.
// On Jetson L4T, libcamera has no Tegra pipeline handler (cam --list is empty)
// and plain v4l2src cannot drive the raw-Bayer VI pipeline, so nvarguscamerasrc
// (sensor -> ISP -> NVMM NV12) is the only working GStreamer source.
func useArgusSource(transport camera.Transport, isJetson bool, available map[string]bool) bool {
	return transport == camera.TransportCSI && isJetson && available != nil && available["nvarguscamerasrc"]
}

// argusDefault* are the capture dimensions used when the request leaves width,
// height, or framerate at 0 (otherwise Argus selects the sensor's largest mode).
const (
	argusDefaultWidth     = 1920
	argusDefaultHeight    = 1080
	argusDefaultFramerate = 30
)

// buildArgusGStreamerArgs builds a gst-launch-1.0 pipeline that captures from a
// Jetson CSI sensor via nvarguscamerasrc (ISP-processed NV12 in NVMM memory) and
// encodes to H.264. The nvv4l2h264enc hardware encoder consumes NVMM NV12
// directly (zero-copy); any other encoder needs frames copied to system memory
// first via nvvidconv. sensorID is the Argus sensor index (derived from the
// /dev/videoN suffix by the caller; correct for the common single-CSI-camera
// case).
func buildArgusGStreamerArgs(gstPath string, req *agentpb.StreamVideoRequest, sensorID int, encoder string, hasH264Parse bool, available map[string]bool) []string {
	width := req.GetWidth()
	if width == 0 {
		width = argusDefaultWidth
	}
	height := req.GetHeight()
	if height == 0 {
		height = argusDefaultHeight
	}
	framerate := req.GetFramerate()
	if framerate == 0 {
		framerate = argusDefaultFramerate
	}
	nvmmCaps := fmt.Sprintf("video/x-raw(memory:NVMM),width=%d,height=%d,framerate=%d/1,format=NV12", width, height, framerate)
	gop := keyframeIntervalFrames(req.GetFramerate())

	var tail string
	if encoder == "nvv4l2h264enc" {
		// HW encoder accepts NVMM NV12 directly — no copy to system memory.
		// keyframeArg caps the keyframe interval (iframeinterval) so a dropped
		// frame self-heals quickly, matching the buildGStreamerArgs path.
		tail = "nvv4l2h264enc" + keyframeArg("nvv4l2h264enc", gop)
		if hasH264Parse {
			tail += h264ByteStream
		}
	} else {
		// Any other encoder needs frames in system memory; nvvidconv does the
		// NVMM->CPU copy, then the shared encoderSegment handles the rest.
		tail = "nvvidconv ! video/x-raw,format=NV12 ! " + encoderSegment(encoder, hasH264Parse, gop)
	}

	pipeline := fmt.Sprintf("nvarguscamerasrc sensor-id=%d ! %s ! %s ! fdsink fd=1", sensorID, nvmmCaps, tail)
	// -q matches buildGStreamerArgs: suppress gst-launch status text so it does
	// not corrupt the binary H.264 stream on stdout.
	return append([]string{gstPath, "-q"}, strings.Fields(pipeline)...)
}

// h264ByteStream normalizes any encoder's H.264 output to Annex B byte-stream
// with in-band, per-keyframe SPS/PPS (config-interval=-1).
//
// Without it, encoders such as x264enc default to stream-format=avc when piped
// to fdsink (its src caps list "avc" before "byte-stream", and fdsink imposes no
// constraint). AVC carries SPS/PPS out-of-band in the caps codec_data, which is
// discarded when the elementary stream is piped raw over gRPC. The client's
// `fdsrc ! typefind ! h264parse` pipeline then sees length-prefixed NALs with no
// start codes and fails with "Could not determine type of stream". Annex B with
// repeated SPS/PPS also lets the client sync mid-stream.
const h264ByteStream = " ! h264parse config-interval=-1 ! video/x-h264,stream-format=byte-stream,alignment=au"

func keyframeArg(encoder string, gop int) string {
	switch encoder {
	case "x264enc":
		// bframes=0 is implied by tune=zerolatency; set it explicitly so the
		// decoder's frame-reorder depth is provably 0.
		return fmt.Sprintf(" key-int-max=%d bframes=0", gop)
	case "nvv4l2h264enc":
		return fmt.Sprintf(" iframeinterval=%d", gop)
	case "avenc_h264", "openh264enc":
		return fmt.Sprintf(" gop-size=%d", gop)
	case "v4l2h264enc":
		// V4L2 M2M encoders take the I-frame period through the extra-controls
		// GStreamer structure property; gst-launch parses the quotes itself.
		// An unknown control name is warned-and-ignored by the v4l2 element,
		// so this is safe even where the driver names the control differently.
		return fmt.Sprintf(" extra-controls=\"controls,h264_i_frame_period=%d\"", gop)
	default:
		return ""
	}
}

// isValidGSTDevicePath reports whether path is a safe V4L2 device node path of the
// form /dev/videoN. Only alphanumeric characters, hyphens, underscores and forward
// slashes are permitted, preventing GStreamer pipeline tokens (!, (, ), ;) and
// whitespace (space, tab) from reaching the gst-launch-1.0 argument string via a
// crafted device path. Whitespace is blocked because buildGStreamerArgs splits the
// constructed pipeline string with strings.Fields — a space in the path would inject
// extra tokens into the argument list.
func isValidGSTDevicePath(path string) bool {
	if len(path) == 0 {
		return false
	}
	for _, c := range path {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '/' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// isValidGSTElementName reports whether name is a safe GStreamer element identifier.
// GStreamer element names are restricted to letters, digits, underscores and hyphens;
// any other character (including pipeline tokens !, (, ), ; and whitespace) would
// enable pipeline injection when the name is interpolated into a gst-launch-1.0
// argument string that is subsequently split with strings.Fields.
func isValidGSTElementName(name string) bool {
	if len(name) == 0 {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// encoderSegment returns the GStreamer pipeline segment for the given encoder element.
// H.264 encoders force I420 (4:2:0) input to avoid 4:4:4 output paths that can make
// encoders such as x264enc select profile 244 (High 4:4:4 Predictive), which
// VideoToolbox and most hardware decoders reject. This input cap does not by itself
// enforce a specific H.264 output profile; explicit profile caps are added only where needed
// (for example, v4l2h264enc is capped to baseline below).
func encoderSegment(encoder string, hasH264Parse bool, gop int) string {
	if encoder == "vp8enc" {
		// webmmux streamable=true writes headers that matroskademux can parse from a pipe.
		// keyframe-max-dist caps the GOP so a dropped frame self-heals quickly.
		return fmt.Sprintf("videoconvert ! vp8enc deadline=1 keyframe-max-dist=%d ! webmmux streamable=true", gop)
	}

	kf := keyframeArg(encoder, gop)

	var enc string
	switch encoder {
	case "nvv4l2h264enc":
		// Jetson L4T hardware encoder: NV12 only in NVMM memory. videoconvert emits system
		// memory, so nvvidconv must move the frames across or the pipeline never links.
		enc = "videoconvert ! video/x-raw,format=NV12 ! nvvidconv ! " +
			"video/x-raw(memory:NVMM),format=NV12 ! nvv4l2h264enc" + kf
	case "v4l2h264enc":
		// The Raspberry Pi bcm2835-codec rejects frames ("Failed to process
		// frame") unless the output H.264 level is pinned — a bare or
		// profile-only capsfilter lets it negotiate a level the driver can't
		// process. level 4 covers up to 1080p30, the encoder's ceiling
		// (verified on a Pi 4, WDY-1603).
		enc = "videoconvert ! video/x-raw,format=I420 ! v4l2h264enc" + kf + " ! video/x-h264,profile=baseline,level=(string)4"
	case "x264enc":
		enc = "videoconvert ! video/x-raw,format=I420 ! x264enc tune=zerolatency" + kf + " ! video/x-h264,profile=high"
	case "openh264enc":
		enc = "videoconvert ! video/x-raw,format=I420 ! openh264enc" + kf
	case "avenc_h264":
		enc = "videoconvert ! video/x-raw,format=I420 ! avenc_h264" + kf
	default:
		// For other H.264-family encoders, force I420 to avoid 4:4:4 profile selection.
		enc = "videoconvert ! video/x-raw,format=I420 ! " + encoder + kf
	}
	if hasH264Parse {
		return enc + h264ByteStream
	}
	return enc
}
