package services

// The native RealSense calibrated-frame source: no vendor SDK, no helper
// process, nothing to distribute beyond the agent
// (specs/2026-10-12-realsense-native-rgbd-design.md).
//
// Three transports, each already owned by the agent:
//
//   - COLOUR is never captured here. The source subscribes to the same
//     deviceHub raw tap every other consumer shares, so viewers and calibrated
//     subscribers coexist on one open of the colour node — the lockout the
//     vendor SDK imposed (it claims every node of the camera) has no native
//     equivalent.
//   - DEPTH is the agent's own V4L2 capture of the Z16 node
//     (captureDepthZ16), a node no app has a legitimate direct use for.
//   - CALIBRATION is the pure-Go HWM read (internal/agent/realsense),
//     factory tables cached per serial, the writable depth units re-read at
//     every capture start.
//
// Pairing is by agent-receipt wall clock, nearest depth frame within half the
// depth frame interval — the same timestamp-matching policy librealsense's
// syncer applies, since D4xx colour and depth are not genlocked. Alignment
// runs once per published frame, here in the producer, never per subscriber.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/framesource"
	"github.com/wendylabsinc/wendy/go/internal/agent/realsense"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// Default capture geometry when a subscriber names nothing. 1280x720 colour is
// the largest mode whose colour+depth frame fits a default gRPC receive limit.
const (
	defaultColourWidth  = 1280
	defaultColourHeight = 720
)

// firstColourFrameTimeout bounds how long Open waits for the colour pipeline to
// deliver its first frame, which is when the negotiated format is known. The
// GStreamer pipeline usually starts in low single-digit seconds.
const firstColourFrameTimeout = 15 * time.Second

// depthRingSize is how many recent depth frames are kept for pairing. At
// matched framerates one or two suffice; four tolerates scheduling jitter
// without meaningfully aging the pair.
const depthRingSize = 4

// NewRealSenseProvider enumerates RealSense cameras from sysfs and serves
// calibrated frames from them natively. sysfsRoot "" means the live system;
// tests pass a fixture tree.
func NewRealSenseProvider(logger *zap.Logger, video *VideoService, sysfsRoot string) framesource.Provider {
	p := &realsenseProvider{logger: logger, video: video, sysfsRoot: sysfsRoot, cals: map[string]*realsense.Calibration{}}
	return framesource.ProviderFunc(p.sources)
}

type realsenseProvider struct {
	logger    *zap.Logger
	video     *VideoService
	sysfsRoot string

	mu   sync.Mutex
	cals map[string]*realsense.Calibration // factory tables, cached per serial
}

func (p *realsenseProvider) sources(ctx context.Context) ([]framesource.Source, error) {
	cams, err := realsense.Detect(p.sysfsRoot)
	if err != nil {
		return nil, err
	}
	out := make([]framesource.Source, 0, len(cams))
	for _, cam := range cams {
		out = append(out, &realsenseSource{provider: p, cam: cam})
	}
	return out, nil
}

// calibration reads the factory tables once per serial and caches them: they
// are burned in at the factory, and the one runtime value (depth units) is
// re-read fresh by every Open. The read is budgeted inside the transport —
// callers never retry it in a loop.
func (p *realsenseProvider) calibration(cam realsense.Camera) (*realsense.Calibration, error) {
	p.mu.Lock()
	cached := p.cals[cam.Serial]
	p.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	cal, err := p.readCalibration(cam)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.cals[cam.Serial] = cal
	p.mu.Unlock()
	return cal, nil
}

func (p *realsenseProvider) readCalibration(cam realsense.Camera) (*realsense.Calibration, error) {
	t, err := realsense.OpenTransport(cam.DepthNode)
	if err != nil {
		return nil, err
	}
	defer t.Close() //nolint:errcheck
	return realsense.ReadCalibration(t)
}

type realsenseSource struct {
	provider *realsenseProvider
	cam      realsense.Camera
}

func (s *realsenseSource) name() string { return framesource.KindRealSense + ":" + s.cam.Serial }

func (s *realsenseSource) description() string {
	if s.cam.Model != "" {
		return "Intel RealSense " + s.cam.Model
	}
	return fmt.Sprintf("Intel RealSense (unrecognised model %s)", s.cam.PID)
}

// Describe reports identity and promises without disturbing any capture:
// enumeration is sysfs reads, and the calibration is a cached budgeted read of
// UVC control requests, which coexist with streaming.
func (s *realsenseSource) Describe(ctx context.Context) (*agentpbv2.CalibratedSource, error) {
	desc := &agentpbv2.CalibratedSource{
		Source:       s.name(),
		Kind:         framesource.KindRealSense,
		Description:  s.description(),
		ColourWidth:  defaultColourWidth,
		ColourHeight: defaultColourHeight,
		ColourFourcc: "YUYV",
		Available:    true,
	}
	cal, err := s.provider.calibration(s.cam)
	if err != nil {
		desc.Available = false
		desc.UnavailableReason = fmt.Sprintf("factory calibration could not be read: %v", err)
		return desc, nil
	}
	if s.cam.ColourNode == "" {
		desc.Available = false
		desc.UnavailableReason = "the camera exposes no colour function"
		return desc, nil
	}
	if s.cam.Model == "" {
		// Unknown models enumerate and refuse depth by name, so a new variant
		// fails visibly rather than wrongly.
		desc.UnavailableReason = fmt.Sprintf("unrecognised RealSense model %s: depth disabled", s.cam.PID)
		desc.Provides = []agentpbv2.FrameRequirement{agentpbv2.FrameRequirement_FRAME_REQUIREMENT_MEASURED_INTRINSICS}
		desc.MaxFrameBytes = framesource.FrameBytes(defaultColourWidth*2, defaultColourHeight, 0, 0)
		return desc, nil
	}
	dw, dh := depthModeFor(defaultColourWidth, defaultColourHeight)
	desc.DepthWidth, desc.DepthHeight = defaultColourWidth, defaultColourHeight // aligned to colour
	desc.MaxFrameBytes = framesource.FrameBytes(defaultColourWidth*2, defaultColourHeight, defaultColourWidth*2, defaultColourHeight)
	desc.Provides = []agentpbv2.FrameRequirement{
		agentpbv2.FrameRequirement_FRAME_REQUIREMENT_MEASURED_INTRINSICS,
	}
	if _, err := cal.DepthIntrinsicsFor(dw, dh); err == nil && cal.BaselineMM != 0 {
		desc.Provides = append(desc.Provides, agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH)
	}
	return desc, nil
}

// depthModeFor picks the camera's native depth mode for a colour aspect
// ratio. Depth is never forced to the colour resolution (every D4xx tops out
// at 1280x720); alignment resamples it onto the colour grid.
func depthModeFor(colourW, colourH uint32) (uint32, uint32) {
	if colourW*3 == colourH*4 {
		return 640, 480
	}
	return 848, 480 // 16:9 and everything else
}

// Open starts (or joins) the shared colour capture, starts the depth capture,
// and blocks until the first colour frame so the negotiated format is known
// and open failures surface here rather than on a later Next.
func (s *realsenseSource) Open(ctx context.Context, opts framesource.Options) (framesource.Stream, error) {
	cal, err := s.provider.calibration(s.cam)
	if err != nil {
		return nil, fmt.Errorf("factory calibration could not be read: %w", err)
	}
	// The one writable calibration value, fresh for this capture: an
	// advanced-mode preset may have changed it since the last one.
	fresh, err := s.provider.readCalibration(s.cam)
	if err == nil {
		cal = fresh
		s.provider.mu.Lock()
		s.provider.cals[s.cam.Serial] = fresh
		s.provider.mu.Unlock()
	}

	colourW, colourH := uint32(defaultColourWidth), uint32(defaultColourHeight)
	if opts.Width != 0 {
		colourW = opts.Width
	}
	if opts.Height != 0 {
		colourH = opts.Height
	}

	st := &realsenseStream{
		source: s,
		cal:    cal,
		video:  s.provider.video,
		logger: s.provider.logger,
	}
	st.ctx, st.cancel = context.WithCancel(context.WithoutCancel(ctx))

	// Depth first: it warms up (S_FMT, buffers, STREAMON) while the colour
	// pipeline starts, so the first colour frame usually has a partner
	// waiting. Absence of depth support is honest, not fatal.
	if s.cam.Model != "" && cal.DepthScaleM > 0 {
		dw, dh := depthModeForOpts(opts)
		if di, err := cal.DepthIntrinsicsFor(dw, dh); err == nil {
			st.depthIntrin, st.depthEnabled = di, true
			fps := opts.Framerate
			if fps == 0 {
				fps = 30
			}
			// One full frame interval: receipt clocks on the two paths differ
			// by pipeline latency, and existence of a pair is what the window
			// gates — depth arrives every interval, so this always pairs once
			// both captures flow.
			st.pairWindow = time.Second / time.Duration(fps)
			st.warmupUntil = time.Now().Add(depthWarmup)
			st.depthDone = make(chan struct{})
			go st.runDepth(dw, dh, fps)
		}
	}

	// Colour: join the shared hub as a raw subscriber — the same path a
	// `camera view --raw` takes, so one open of the node serves everyone.
	req := &agentpb.StreamVideoRequest{
		Codec:     agentpb.VideoCodec_VIDEO_CODEC_RAW,
		Width:     opts.Width,
		Height:    opts.Height,
		Framerate: opts.Framerate,
	}
	hub, id, ch, err := s.provider.video.getOrCreateHub(ctx, s.cam.ColourNode, req, "calibrated:"+s.name())
	if err != nil {
		st.cancel()
		return nil, err
	}
	st.hub, st.subID, st.colour = hub, id, ch

	// Wait for the first colour frame: it carries the negotiated raw format,
	// and a pipeline that cannot start reports here instead of hanging a
	// subscriber. The frame is kept for the first Next.
	first, err := st.awaitFirstColour(ctx)
	if err != nil {
		st.Close()
		return nil, err
	}
	st.pending = first
	cw := first.rawFmt.GetWidth()
	chg := first.rawFmt.GetHeight()
	if cw != 0 {
		colourW, colourH = cw, chg
	}
	st.colourIntrin, err = cal.ColourIntrinsicsFor(colourW, colourH)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("no factory colour model for %dx%d: %w", colourW, colourH, err)
	}

	return st, nil
}

// depthModeForOpts picks the depth mode for the colour geometry a subscriber
// asked for, defaulting to the 16:9 default mode.
func depthModeForOpts(opts framesource.Options) (uint32, uint32) {
	w, h := uint32(defaultColourWidth), uint32(defaultColourHeight)
	if opts.Width != 0 && opts.Height != 0 {
		w, h = opts.Width, opts.Height
	}
	return depthModeFor(w, h)
}

// depthWarmup bounds how long the first frames wait for the depth capture to
// produce its first partner. Within it, a colour frame with no pair is
// SKIPPED rather than published depth-less: publishing it would end every
// subscriber that required depth over a startup race rather than a fact
// about the camera.
const depthWarmup = 3 * time.Second

type realsenseStream struct {
	source *realsenseSource
	cal    *realsense.Calibration
	video  *VideoService
	logger *zap.Logger

	ctx    context.Context
	cancel context.CancelFunc

	hub    *deviceHub
	subID  int
	colour chan *videoFrame

	colourIntrin realsense.Intrinsics
	depthIntrin  realsense.Intrinsics
	depthEnabled bool
	pairWindow   time.Duration
	warmupUntil  time.Time
	depthDone    chan struct{}

	pending *videoFrame // the first frame, consumed by the first Next

	mu       sync.Mutex
	ring     []depthFrame // most recent last
	depthErr error

	frameID   uint64
	closeOnce sync.Once
}

type depthFrame struct {
	data   []byte
	wallNs uint64
}

func (st *realsenseStream) awaitFirstColour(ctx context.Context) (*videoFrame, error) {
	timer := time.NewTimer(firstColourFrameTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, fmt.Errorf("the colour pipeline produced no frame within %s", firstColourFrameTimeout)
		case frame, ok := <-st.colour:
			if !ok {
				if err := st.hub.subscriberErr(st.subID); err != nil {
					return nil, err
				}
				return nil, errors.New("the colour capture ended before its first frame")
			}
			if frame.rawFmt == nil {
				continue // not a raw frame; should not happen for a raw subscriber
			}
			return frame, nil
		}
	}
}

// runDepth owns the Z16 capture for this stream's lifetime, keeping the most
// recent frames for pairing. Its exit (camera unplugged, node claimed by a
// vendor-SDK app) poisons the stream so Next reports it rather than quietly
// publishing colour-only frames to subscribers that required depth.
func (st *realsenseStream) runDepth(dw, dh, fps uint32) {
	defer close(st.depthDone)
	err := st.video.captureDepthZ16(st.ctx, func(data []byte, stamp frameTimestamp) bool {
		st.mu.Lock()
		if len(st.ring) == depthRingSize {
			copy(st.ring, st.ring[1:])
			st.ring = st.ring[:depthRingSize-1]
		}
		st.ring = append(st.ring, depthFrame{data: data, wallNs: stamp.wallNs})
		st.mu.Unlock()
		return true
	}, st.source.cam.DepthNode, dw, dh, fps)
	if err != nil && st.ctx.Err() == nil {
		st.mu.Lock()
		st.depthErr = fmt.Errorf("the depth capture ended: %w", err)
		st.mu.Unlock()
	}
}

// pairDepth finds the depth frame nearest the colour instant, within the
// window. A miss is an honest no-depth frame, which ends any subscriber that
// required depth — never a stale measurement passed off as current.
func (st *realsenseStream) pairDepth(colourNs uint64) ([]byte, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.depthErr != nil {
		return nil, st.depthErr
	}
	var best []byte
	bestDelta := uint64(st.pairWindow.Nanoseconds())
	for _, d := range st.ring {
		delta := d.wallNs - colourNs
		if d.wallNs < colourNs {
			delta = colourNs - d.wallNs
		}
		if delta <= bestDelta {
			best, bestDelta = d.data, delta
		}
	}
	return best, nil
}

func (st *realsenseStream) Next(ctx context.Context) (*agentpbv2.CalibratedFrame, error) {
	var frame *videoFrame
	if st.pending != nil {
		frame, st.pending = st.pending, nil
	} else {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-st.ctx.Done():
			return nil, context.Canceled
		case f, ok := <-st.colour:
			if !ok {
				if err := st.hub.subscriberErr(st.subID); err != nil {
					return nil, err
				}
				return nil, io.EOF
			}
			if f.rawFmt == nil {
				return nil, fmt.Errorf("the colour hub delivered a non-raw frame to a raw subscriber")
			}
			frame = f
		}
	}

	st.frameID++
	out := &agentpbv2.CalibratedFrame{
		FrameId:      st.frameID,
		CapturedAtNs: frame.tsNs,
		Colour:       frame.data,
		ColourFormat: frame.rawFmt,
		Intrinsics: &agentpbv2.CameraIntrinsics{
			Fx: float32(st.colourIntrin.Fx), Fy: float32(st.colourIntrin.Fy),
			Cx: float32(st.colourIntrin.Ppx), Cy: float32(st.colourIntrin.Ppy),
			Width: st.colourIntrin.Width, Height: st.colourIntrin.Height,
			Provenance: agentpbv2.CameraIntrinsics_PROVENANCE_MEASURED,
			Note: fmt.Sprintf("factory calibration, serial %s, fw %s",
				st.source.cam.Serial, st.cal.FirmwareVersion),
		},
		Source: st.source.name(),
	}
	if st.depthEnabled {
		depth, err := st.pairDepth(frame.tsNs)
		if err != nil {
			return nil, err
		}
		if depth == nil && time.Now().Before(st.warmupUntil) {
			// Startup race, not a camera fact: wait for the partner.
			st.frameID--
			return st.Next(ctx)
		}
		if depth != nil {
			aligned, err := st.cal.AlignFrame(depth, st.depthIntrin, st.colourIntrin)
			if err != nil {
				return nil, fmt.Errorf("aligning depth to colour: %w", err)
			}
			out.Depth = &agentpbv2.DepthPlane{
				Data:         aligned,
				Width:        st.colourIntrin.Width,
				Height:       st.colourIntrin.Height,
				BytesPerLine: st.colourIntrin.Width * 2,
				ScaleM:       float32(st.cal.DepthScaleM),
				Alignment:    agentpbv2.DepthPlane_ALIGNMENT_ALIGNED_TO_COLOUR,
			}
		}
	}
	return out, nil
}

// Negotiated reports what this capture actually opened — the descriptor the
// service re-checks every subscriber against, including the first one.
func (st *realsenseStream) Negotiated() *agentpbv2.CalibratedSource {
	desc := &agentpbv2.CalibratedSource{
		Source:       st.source.name(),
		Kind:         framesource.KindRealSense,
		Description:  st.source.description(),
		ColourWidth:  st.colourIntrin.Width,
		ColourHeight: st.colourIntrin.Height,
		ColourFourcc: "YUYV",
		Available:    true,
		Provides:     []agentpbv2.FrameRequirement{agentpbv2.FrameRequirement_FRAME_REQUIREMENT_MEASURED_INTRINSICS},
		MaxFrameBytes: framesource.FrameBytes(
			st.colourIntrin.Width*2, st.colourIntrin.Height,
			st.colourIntrin.Width*2, st.colourIntrin.Height),
	}
	if st.depthEnabled {
		desc.DepthWidth, desc.DepthHeight = st.colourIntrin.Width, st.colourIntrin.Height
		desc.Provides = append(desc.Provides, agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH)
	} else {
		desc.MaxFrameBytes = framesource.FrameBytes(st.colourIntrin.Width*2, st.colourIntrin.Height, 0, 0)
	}
	return desc
}

// Close is safe to call more than once and concurrently with Next: it cancels
// the stream context (unblocking Next and ending the depth capture) and
// leaves the shared colour hub, whose producer stops only when its LAST
// subscriber leaves — a viewer watching the same camera keeps it.
func (st *realsenseStream) Close() error {
	st.closeOnce.Do(func() {
		st.cancel()
		st.hub.unsubscribe(st.subID)
		if st.depthDone != nil {
			<-st.depthDone // the depth node is free when Close returns
		}
	})
	return nil
}
