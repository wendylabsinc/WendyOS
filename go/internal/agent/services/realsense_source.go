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
// Pairing is by agent-receipt wall clock, nearest depth frame within one depth
// frame interval, since D4xx colour and depth are not genlocked. The two
// receipt clocks are not capture clocks: colour is stamped after the
// GStreamer pipeline and depth at DQBUF, so the window is wide enough to
// guarantee a pair, and the pair can be up to a frame apart in capture time.
// Alignment runs once per published frame, here in the producer, never per
// subscriber.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"

	"github.com/wendylabsinc/wendy/go/internal/agent/framesource"
	"github.com/wendylabsinc/wendy/go/internal/agent/realsense"
	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
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

// calibrationRetryInterval spaces repeat reads of a camera whose last read
// failed, and of a depth scale that could not be read. Every listing and every
// lookup describes every source, and clients poll; without it each of those
// would repeat the HWM exchange on a control path that has to stay budgeted.
const calibrationRetryInterval = 10 * time.Second

// NewRealSenseProvider enumerates RealSense cameras from sysfs and serves
// calibrated frames from them natively. sysfsRoot "" means the live system;
// tests pass a fixture tree.
func NewRealSenseProvider(logger *zap.Logger, video *VideoService, sysfsRoot string) framesource.Provider {
	return framesource.ProviderFunc(newRealSenseProvider(logger, video, sysfsRoot).sources)
}

func newRealSenseProvider(logger *zap.Logger, video *VideoService, sysfsRoot string) *realsenseProvider {
	return &realsenseProvider{
		logger: logger, video: video, sysfsRoot: sysfsRoot,
		cals:      map[string]*calibrationEntry{},
		readCal:   hwmReadCalibration,
		readScale: hwmReadDepthScale,
	}
}

type realsenseProvider struct {
	logger    *zap.Logger
	video     *VideoService
	sysfsRoot string

	// readCal and readScale are the two HWM conversations: the full calibration
	// read and the depth-units re-read. Seams, so tests can stand in for a
	// camera.
	readCal   func(realsense.Camera) (*realsense.Calibration, error)
	readScale func(realsense.Camera) (float64, error)

	mu   sync.Mutex
	cals map[string]*calibrationEntry // per serial; entries are never removed
}

// calibrationEntry is one camera's calibration state. Its mutex is held across
// every HWM conversation with that camera: a command's reply is read back from
// the same extension-unit control it was written to, so two conversations
// interleaved on one camera would each read the other's reply.
type calibrationEntry struct {
	mu     sync.Mutex
	cal    *realsense.Calibration // nil until a read succeeds; replaced, never mutated
	err    error                  // the last failed read, while cal is nil
	readAt time.Time              // when the last read ran, full or depth units only
}

func (p *realsenseProvider) entry(serial string) *calibrationEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.cals[serial]
	if e == nil {
		e = &calibrationEntry{}
		p.cals[serial] = e
	}
	return e
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
// are burned in at the factory. The depth units it carries are as of the last
// read; Open re-reads them for every capture. A failed read is not repeated,
// and a zero depth scale not re-read, within calibrationRetryInterval. The
// read is budgeted inside the transport — callers never retry it in a loop.
func (p *realsenseProvider) calibration(cam realsense.Camera) (*realsense.Calibration, error) {
	e := p.entry(cam.Serial)
	e.mu.Lock()
	defer e.mu.Unlock()
	if time.Since(e.readAt) >= calibrationRetryInterval {
		switch {
		case e.cal == nil:
			e.cal, e.err = p.readCal(cam)
			e.readAt = time.Now()
		case e.cal.DepthScaleM <= 0:
			_ = p.refreshDepthScaleLocked(e, cam) // a failure leaves it at 0, which Describe reports
		}
	}
	if e.cal == nil {
		return nil, e.err
	}
	return e.cal, nil
}

// freshDepthScale re-reads the depth units of a camera whose factory tables
// are cached, for a capture about to start. A failed read stores and returns
// a zero scale beside the error: depth in unknown units is refused, never
// carried over from an older read.
func (p *realsenseProvider) freshDepthScale(cam realsense.Camera) (*realsense.Calibration, error) {
	e := p.entry(cam.Serial)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cal == nil {
		return nil, errors.New("factory calibration has not been read")
	}
	err := p.refreshDepthScaleLocked(e, cam)
	return e.cal, err
}

// refreshDepthScaleLocked replaces e.cal with a copy carrying a freshly read
// depth scale. A copy, because running streams hold the old one. e.mu is held.
func (p *realsenseProvider) refreshDepthScaleLocked(e *calibrationEntry, cam realsense.Camera) error {
	scale, err := p.readScale(cam)
	cal := *e.cal
	cal.DepthScaleM = scale
	e.cal, e.readAt = &cal, time.Now()
	return err
}

func hwmReadCalibration(cam realsense.Camera) (*realsense.Calibration, error) {
	t, err := realsense.OpenTransport(cam.DepthNode)
	if err != nil {
		return nil, err
	}
	defer t.Close() //nolint:errcheck
	return realsense.ReadCalibration(t, cam.PID)
}

func hwmReadDepthScale(cam realsense.Camera) (float64, error) {
	t, err := realsense.OpenTransport(cam.DepthNode)
	if err != nil {
		return 0, err
	}
	defer t.Close() //nolint:errcheck
	return realsense.ReadDepthScale(t, cam.PID)
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
	desc.Provides = []agentpbv2.FrameRequirement{agentpbv2.FrameRequirement_FRAME_REQUIREMENT_MEASURED_INTRINSICS}
	dw, dh := depthModeFor(defaultColourWidth, defaultColourHeight)
	if why := s.depthUnavailable(cal, dw, dh); why != "" {
		// Depth is refused by name, so a new variant or an unreadable
		// setting fails visibly rather than wrongly.
		desc.UnavailableReason = why
		desc.MaxFrameBytes = framesource.FrameBytes(defaultColourWidth*2, defaultColourHeight, 0, 0)
		return desc, nil
	}
	desc.DepthWidth, desc.DepthHeight = defaultColourWidth, defaultColourHeight // aligned to colour
	desc.MaxFrameBytes = framesource.FrameBytes(defaultColourWidth*2, defaultColourHeight, defaultColourWidth*2, defaultColourHeight)
	desc.Provides = append(desc.Provides, agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH)
	return desc, nil
}

// depthUnavailable says why a capture in depth mode dw x dh cannot carry
// aligned depth, or "" when it can. Describe and Open both gate on it, so the
// listing never promises depth a capture would then go without.
func (s *realsenseSource) depthUnavailable(cal *realsense.Calibration, dw, dh uint32) string {
	switch {
	case s.cam.Model == "":
		return fmt.Sprintf("unrecognised RealSense model %s: depth disabled", s.cam.PID)
	case cal.DepthScaleM <= 0:
		return "the camera's depth units could not be read: depth disabled"
	case cal.BaselineMM == 0:
		return "the factory calibration carries no stereo baseline: depth disabled"
	}
	if _, err := cal.DepthIntrinsicsFor(dw, dh); err != nil {
		return fmt.Sprintf("%v: depth disabled", err)
	}
	return ""
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
	if _, err := s.provider.calibration(s.cam); err != nil {
		return nil, fmt.Errorf("factory calibration could not be read: %w", err)
	}
	// The one writable calibration value, fresh for this capture: an
	// advanced-mode preset may have changed it since the last one. Two
	// exchanges, not the whole read; a failure disables depth for this
	// capture, by name, rather than reusing an older value.
	cal, scaleErr := s.provider.freshDepthScale(s.cam)
	if cal == nil {
		return nil, fmt.Errorf("factory calibration could not be read: %w", scaleErr)
	}
	if scaleErr != nil {
		s.provider.logger.Warn("RealSense depth units could not be read; this capture has no depth",
			zap.String("source", s.name()), zap.Error(scaleErr))
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
	// waiting. Absence of depth support is honest, not fatal: Negotiated
	// reports why, and a subscriber that required depth is refused with it.
	dw, dh := depthModeForOpts(opts)
	if st.depthOff = s.depthUnavailable(cal, dw, dh); st.depthOff == "" {
		st.depthIntrin, _ = cal.DepthIntrinsicsFor(dw, dh)
		st.depthEnabled = true
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
	depthOff     string // why depth is not enabled, when it is not
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
				if err := st.colourEnded(); !errors.Is(err, io.EOF) {
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
	planeBytes := int(dw) * int(dh) * 2
	dropped := 0
	err := st.video.captureDepthZ16(st.ctx, func(data []byte, stamp frameTimestamp) bool {
		// A buffer the driver flagged as damaged, or one that is not exactly
		// the Z16 plane of this mode, is a USB hiccup, not a measurement.
		// Dropped here it costs one pairing; passed on, alignment would refuse
		// it and end the capture for every subscriber.
		if stamp.nativeFlags&v4l2BufFlagError != 0 || len(data) != planeBytes {
			if dropped++; dropped == 1 {
				st.logger.Warn("dropping damaged RealSense depth frames",
					zap.String("source", st.source.name()), zap.Int("bytes", len(data)),
					zap.Int("want", planeBytes), zap.Bool("driver_error", stamp.nativeFlags&v4l2BufFlagError != 0))
			}
			return true
		}
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
	for {
		frame, err := st.nextColour(ctx)
		if err != nil {
			return nil, err
		}
		var depth []byte
		if st.depthEnabled {
			if depth, err = st.pairDepth(frame.tsNs); err != nil {
				return nil, err
			}
			if depth == nil && time.Now().Before(st.warmupUntil) {
				continue // Startup race, not a camera fact: wait for the partner.
			}
		}
		return st.assemble(frame, depth)
	}
}

// nextColour is the next colour frame: the one Open waited for, then the
// shared hub's.
func (st *realsenseStream) nextColour(ctx context.Context) (*videoFrame, error) {
	if frame := st.pending; frame != nil {
		st.pending = nil
		return frame, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-st.ctx.Done():
		return nil, context.Canceled
	case f, ok := <-st.colour:
		if !ok {
			return nil, st.colourEnded()
		}
		if f.rawFmt == nil {
			return nil, fmt.Errorf("the colour hub delivered a non-raw frame to a raw subscriber")
		}
		return f, nil
	}
}

// colourEnded says why the shared colour hub closed this subscriber. Only a
// producer that finished cleanly is a clean end (io.EOF). A failed capture,
// or one an episode capture took over at its campaign's parameters, ends the
// calibrated stream with that reason — the same answer StreamVideo gives —
// rather than an end subscribers would read as the camera simply finishing.
func (st *realsenseStream) colourEnded() error {
	if err := st.hub.subscriberErr(st.subID); err != nil {
		return err
	}
	if err := st.hub.terminalErr(); err != nil {
		return err
	}
	if st.hub.wasRestarted() {
		return streamreason.New(codes.Unavailable,
			"an episode capture restarted the colour capture at the campaign's requested parameters; resubscribe to join the new stream",
			streamreason.CameraProducerRestarted, nil)
	}
	return io.EOF
}

// assemble builds the published frame from a colour frame and its paired
// depth plane (nil for none), aligning depth onto the colour grid.
func (st *realsenseStream) assemble(frame *videoFrame, depth []byte) (*agentpbv2.CalibratedFrame, error) {
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
		desc.UnavailableReason = st.depthOff
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
