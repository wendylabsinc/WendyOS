package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/oomscore"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const (
	derivedFeedQueueFrames            = 2
	derivedTeeName                    = "derived"
	derivedRestartPause               = 500 * time.Millisecond
	derivedFailedStartsBeforeGivingUp = 5
)

// derivedTransform is everything a derived layer knows about one native
// format. A camera type whose format is new adds an entry to derivedTransforms
// and nothing else here.
type derivedTransform struct {
	inputCaps func(width, height, framerate uint32) string
	// decoders are the ways to decode the format, the preferred one first.
	decoders      []derivedDecoder
	pictureStages string
	pictures      func(width, height uint32) *agentpb.RawFormat
	rawWhy        func(width, height uint32) string
	// encodes adds the H.264 branch. A format that is H.264 already has none:
	// viewers and recordings get the camera's own frames.
	encodes bool
	// reorders reports a frame whose picture a decoder hands back out of turn.
	// GStreamer's decoders then move timestamps between pictures, so no picture
	// of such a stream can be paired with its frame. Nil when no frame does.
	reorders func(frame []byte) bool
	// holdsPicture reports a frame a decoder gives a picture back for. One
	// that holds none (parameter sets alone) is fed with the next that does: a
	// decoder that pairs each picture with its oldest pending buffer would put
	// every later picture on the wrong frame. Nil when every frame holds one.
	holdsPicture func(frame []byte) bool
}

// derivedDecoder is one way to decode a format: the stages from the framed
// input to pictures in system memory, and the elements they need.
type derivedDecoder struct {
	element  string
	stages   string
	elements []string
}

const h264RawWhy = "camera streams H.264 natively; raw frames are not offered"

var derivedTransforms = map[agentpb.VideoCodec]derivedTransform{
	videoCodecMJPEG: {
		inputCaps: func(width, height, framerate uint32) string {
			return fmt.Sprintf("image/jpeg, width=(int)%d, height=(int)%d, framerate=(fraction)%d/1", width, height, framerate)
		},
		decoders:      []derivedDecoder{{element: "jpegdec", stages: "jpegdec", elements: []string{"jpegdec"}}},
		pictureStages: yuyvPictureStages,
		pictures:      yuyvPictureFormat,
		rawWhy:        mjpegRawWhy,
		encodes:       true,
	},
	agentpb.VideoCodec_VIDEO_CODEC_H264: {
		inputCaps: func(_, _, framerate uint32) string {
			return fmt.Sprintf("video/x-h264, stream-format=(string)byte-stream, alignment=(string)au, framerate=(fraction)%d/1", framerate)
		},
		decoders: []derivedDecoder{
			// nvv4l2decoder hands on NVMM memory, which videoconvert cannot read.
			{element: "nvv4l2decoder", stages: "h264parse ! nvv4l2decoder ! nvvidconv ! video/x-raw", elements: []string{"nvv4l2decoder", "nvvidconv"}},
			{element: "v4l2h264dec", stages: "h264parse ! v4l2h264dec", elements: []string{"v4l2h264dec"}},
			// Frame threads hold back as many pictures as there are threads.
			{element: "avdec_h264", stages: "h264parse ! avdec_h264 thread-type=slice", elements: []string{"avdec_h264"}},
			{element: "openh264dec", stages: "h264parse ! openh264dec", elements: []string{"openh264dec"}},
		},
		pictureStages: yuyvPictureStages,
		pictures:      yuyvPictureFormat,
		rawWhy:        func(uint32, uint32) string { return h264RawWhy },
		reorders:      h264PictureIsBidirectional,
		holdsPicture:  h264HasCodedPicture,
	},
}

var derivedFramingElements = []string{"gdpdepay", "gdppay", "h264parse"}

// derivedLayerDecoder chooses the first of a format's decoders this GStreamer
// can run, or says what it lacks.
func derivedLayerDecoder(transform derivedTransform, available map[string]bool) (derivedDecoder, error) {
	lacking := func(elements []string) (missing []string) {
		for _, element := range elements {
			if !available[element] {
				missing = append(missing, element)
			}
		}
		return missing
	}
	missing := lacking(derivedFramingElements)
	var chosen *derivedDecoder
	var choices []string
	for i, decoder := range transform.decoders {
		if len(lacking(decoder.elements)) == 0 {
			chosen = &transform.decoders[i]
			break
		}
		choices = append(choices, strings.Join(decoder.elements, " with "))
	}
	if chosen == nil && len(transform.decoders) == 1 {
		missing = append(missing, lacking(transform.decoders[0].elements)...)
	}
	sort.Strings(missing)
	if chosen == nil && len(transform.decoders) != 1 {
		missing = append(missing, "a decoder ("+strings.Join(choices, ", ")+")")
	}
	if len(missing) > 0 {
		return derivedDecoder{}, fmt.Errorf("GStreamer lacks %s", strings.Join(missing, ", "))
	}
	return *chosen, nil
}

// derivedLayerEncoder chooses the H.264 encoder a derived layer runs. The
// layer publishes whole access units, which only h264parse's au alignment
// gives, so an encoder chosen for another codec or without the parser is no use.
func derivedLayerEncoder(available map[string]bool) (gstEncoderResult, error) {
	if available == nil {
		return gstEncoderResult{}, errors.New("the installed GStreamer elements could not be listed")
	}
	enc, err := findGStreamerEncoderFromSet(available)
	if err != nil {
		return gstEncoderResult{}, err
	}
	if enc.codec != agentpb.VideoCodec_VIDEO_CODEC_H264 || !enc.hasH264Parse {
		return gstEncoderResult{}, fmt.Errorf("no H.264 encoder with h264parse is installed (found %s)", enc.element)
	}
	if !isValidGSTElementName(enc.element) {
		return gstEncoderResult{}, errors.New("GStreamer encoder name contains invalid characters")
	}
	return enc, nil
}

type derivedLayerGaveUp struct {
	msg string
	// afterPublishing says subscribers already hold frames this layer derived.
	afterPublishing bool
	// lasting says the cause is the camera's stream itself, which another try
	// later meets again.
	lasting bool
}

func (e derivedLayerGaveUp) Error() string { return e.msg }

var (
	errDerivedChildSilent     = errors.New("the pipeline took frames and produced no usable one")
	errDerivedFramesReordered = errors.New("the camera's H.264 has B frames, which a decoder hands back out of order, so no picture can be paired with the frame it came from")
	errDerivedNoStartFrame    = errors.New("the camera sent no frame that decoding can start on")
)

type derivedLayer struct {
	logger    *zap.Logger
	device    string
	format    agentpb.VideoCodec
	transform derivedTransform
	gstPath   string
	decoder   derivedDecoder
	encoder   gstEncoderResult
	grace     time.Duration

	width, height, framerate uint32
	pictures                 *agentpb.RawFormat

	demand  encodeDemand
	publish func(source, derived *videoFrame) (keepProducing bool, err error)

	feed    chan *videoFrame
	feeding atomic.Bool
	tags    derivedFrameTags

	fed, notFed, damaged, awaitedStart, unusable, usable, published, childRuns atomic.Uint64
	failedStarts                                                               int
	// awaitingStart says the child is fed nothing until a frame arrives that
	// decoding can start on.
	awaitingStart atomic.Bool

	// Nanoseconds since created, of the last frame capture offered and of the
	// last usable output; the watch on a child compares them.
	created                   time.Time
	lastOffered, lastUsableAt atomic.Int64

	firstOutputTimeout, idleOutputTimeout time.Duration
	restartPause                          time.Duration
}

type derivedLayerConfig struct {
	logger                   *zap.Logger
	device                   string
	format                   agentpb.VideoCodec
	gstPath                  string
	available                map[string]bool
	width, height, framerate uint32
	grace                    time.Duration
	demand                   encodeDemand
	publish                  func(source, derived *videoFrame) (keepProducing bool, err error)
}

func newDerivedLayer(cfg derivedLayerConfig) (*derivedLayer, error) {
	transform, known := derivedTransforms[cfg.format]
	if !known {
		return nil, fmt.Errorf("no derived frames are defined for %s", codecIndexName(cfg.format))
	}
	decoder, err := derivedLayerDecoder(transform, cfg.available)
	if err != nil {
		return nil, err
	}
	var enc gstEncoderResult
	if transform.encodes {
		if enc, err = derivedLayerEncoder(cfg.available); err != nil {
			return nil, err
		}
	}
	framerate := cfg.framerate
	if framerate == 0 {
		framerate = 30
	}
	return &derivedLayer{
		logger: cfg.logger, device: cfg.device, format: cfg.format, transform: transform,
		gstPath: cfg.gstPath, decoder: decoder, encoder: enc, grace: cfg.grace,
		width: cfg.width, height: cfg.height, framerate: framerate,
		pictures: transform.pictures(cfg.width, cfg.height),
		demand:   cfg.demand, publish: cfg.publish,
		feed:               make(chan *videoFrame, derivedFeedQueueFrames),
		created:            time.Now(),
		firstOutputTimeout: firstFrameTimeout, idleOutputTimeout: cameraIdleTimeout,
		restartPause: derivedRestartPause,
	}, nil
}

// elementInUse names the element a failed child is reported under.
func (l *derivedLayer) elementInUse() string {
	if l.transform.encodes {
		return l.encoder.element
	}
	return l.decoder.element
}

func (l *derivedLayer) sinceCreated() int64 { return int64(time.Since(l.created)) }

// take is the layer's only intake, and the hub's produce its only caller. It
// never blocks the capture that calls it: a frame the child has no room for is
// not fed, and counted.
func (l *derivedLayer) take(frame *videoFrame) {
	if frame.codec != l.format || frame.sampleID == 0 || !l.feeding.Load() {
		return
	}
	l.lastOffered.Store(l.sinceCreated())
	select {
	case l.feed <- frame:
	default:
		l.notFed.Add(1)
	}
}

func (l *derivedLayer) offerTo(sink rawSink) {
	if pixels, ok := sink.(pixelSink); ok && l.pictures != nil {
		pixels.pixelsOffered(l.pictures)
	}
	sink.rawNotOffered(fixedReason(l.transform.rawWhy(l.width, l.height)))
}

func (l *derivedLayer) needs(demand encodeDemand) pipelineShape {
	shape := pipelineShape{pixels: demand.wantRaw()}
	if l.transform.encodes {
		shape = branchesOnDemand(demand)
	}
	shape.pixels = shape.pixels && l.pictures != nil
	return shape
}

func (l *derivedLayer) run(ctx context.Context) error {
	err := runPipelinesShapedBy(ctx, l.demand, l.grace, l.needs, l.launch)
	l.logger.Info("derived layer stopped", append(l.counts().fields(), zap.String("device", l.device))...)
	return err
}

// outputFDs are the child's descriptors for a shape's outputs, from rawTapFD
// up in the order runChild passes them. Neither is fd 1: elements print there.
func outputFDs(shape pipelineShape) (pictures, encoded int) {
	pictures, encoded = rawTapFD, rawTapFD
	if shape.pixels {
		encoded++
	}
	return pictures, encoded
}

func (l *derivedLayer) pipelineArgs(shape pipelineShape) []string {
	picturesFD, encodedFD := outputFDs(shape)
	decode := "fdsrc fd=0 ! gdpdepay ! " + l.decoder.stages
	pictures := fmt.Sprintf("%s ! %s ! gdppay ! fdsink fd=%d", rawTapQueue, l.transform.pictureStages, picturesFD)
	encoded := fmt.Sprintf("%s ! %s ! gdppay ! fdsink fd=%d", leakyRawQueue,
		encoderSegment(l.encoder.element, l.encoder.hasH264Parse, keyframeIntervalFrames(l.framerate)), encodedFD)
	var pipeline string
	switch {
	case shape.encode && shape.pixels:
		pipeline = fmt.Sprintf("%s ! tee name=%s %s. ! %s %s. ! %s",
			decode, derivedTeeName, derivedTeeName, pictures, derivedTeeName, encoded)
	case shape.encode:
		pipeline = decode + " ! " + encoded
	default:
		pipeline = decode + " ! " + pictures
	}
	return append([]string{l.gstPath, "-q"}, strings.Fields(pipeline)...)
}

func (l *derivedLayer) launch(ctx context.Context, shape pipelineShape) error {
	if !shape.encode && !shape.pixels {
		<-ctx.Done()
		return ctx.Err()
	}
	usableBefore := l.usable.Load()
	stopped, err := l.runChild(ctx, shape)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	produced := l.usable.Load() > usableBefore
	switch {
	case errors.Is(stopped, errDerivedFramesReordered):
		return derivedLayerGaveUp{msg: stopped.Error(), afterPublishing: l.published.Load() > 0, lasting: true}
	case errors.Is(stopped, errDerivedNoStartFrame):
		return derivedLayerGaveUp{msg: stopped.Error(), afterPublishing: l.published.Load() > 0}
	case produced:
		l.failedStarts = 0
	case errors.Is(stopped, errDerivedChildSilent):
		return derivedLayerGaveUp{msg: status.Convert(err).Message(), afterPublishing: l.published.Load() > 0}
	default:
		if l.failedStarts++; l.failedStarts >= derivedFailedStartsBeforeGivingUp {
			return derivedLayerGaveUp{afterPublishing: l.published.Load() > 0,
				msg: fmt.Sprintf("derived frame pipeline produced nothing in %d starts: %s", l.failedStarts, status.Convert(err).Message())}
		}
	}
	l.logger.Warn("derived frame pipeline ended; restarting it", zap.String("device", l.device),
		zap.Bool("encodes", shape.encode), zap.Bool("pictures", shape.pixels), zap.Error(err))
	if !produced {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(l.restartPause):
		}
	}
	return fmt.Errorf("%w: %v", errPipelineEndedRestartable, err)
}

type derivedOutput struct {
	name       string
	pipe       *os.File
	maxPayload int
	wanted     func() bool
	usable     func(payloadBytes int64) bool
	frame      func(payload []byte) *videoFrame
}

func (l *derivedLayer) picturesOutput(pipe *os.File) derivedOutput {
	return derivedOutput{name: "pictures", pipe: pipe, maxPayload: maxRawFrameBytes, wanted: l.demand.wantRaw,
		usable: func(payloadBytes int64) bool {
			return payloadBytes == int64(l.pictures.GetBytesPerLine())*int64(l.pictures.GetHeight())
		},
		frame: func(payload []byte) *videoFrame {
			return &videoFrame{data: payload, codec: agentpb.VideoCodec_VIDEO_CODEC_RAW, rawFmt: l.pictures}
		}}
}

func (l *derivedLayer) encodedOutput(pipe *os.File) derivedOutput {
	return derivedOutput{name: "H.264", pipe: pipe, maxPayload: maxFrameBytes, wanted: l.demand.wantEncoded,
		usable: func(payloadBytes int64) bool { return payloadBytes > 0 },
		frame: func(payload []byte) *videoFrame {
			return &videoFrame{data: payload, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: true}
		}}
}

// runChild runs one child until it ends or ctx does. stopped is what ended it
// from this side, nil when ctx did.
func (l *derivedLayer) runChild(ctx context.Context, shape pipelineShape) (stopped, runErr error) {
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := l.pipelineArgs(shape)
	cmd := exec.CommandContext(childCtx, args[0], args[1:]...)
	oomscore.ResetChild(cmd)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C")
	stderr := &limitedBuffer{limit: maxStderrBytes}
	cmd.Stdout, cmd.Stderr = stderr, stderr

	var opened []*os.File
	closeOpened := func() {
		for _, f := range opened {
			f.Close() //nolint:errcheck
		}
	}
	pipe := func() (r, w *os.File, err error) {
		if r, w, err = os.Pipe(); err == nil {
			opened = append(opened, r, w)
		}
		return r, w, err
	}
	stdinR, stdinW, err := pipe()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create derived frame pipe: %v", err)
	}
	cmd.Stdin = stdinR
	childEnds := []*os.File{stdinR}
	var outputs []derivedOutput
	for _, output := range []struct {
		inShape bool
		reads   func(*os.File) derivedOutput
	}{{shape.pixels, l.picturesOutput}, {shape.encode, l.encodedOutput}} {
		if !output.inShape {
			continue
		}
		r, w, err := pipe()
		if err != nil {
			closeOpened()
			return nil, status.Errorf(codes.Internal, "failed to create derived frame pipe: %v", err)
		}
		cmd.ExtraFiles = append(cmd.ExtraFiles, w)
		childEnds = append(childEnds, w)
		outputs = append(outputs, output.reads(r))
	}
	if err := cmd.Start(); err != nil {
		closeOpened()
		return nil, status.Errorf(codes.Internal, "failed to start derived frame pipeline: %v", err)
	}
	for _, f := range childEnds {
		f.Close() //nolint:errcheck
	}
	l.childRuns.Add(1)
	l.logger.Info("derived frame pipeline started", zap.String("device", l.device), zap.String("decoder", l.decoder.element),
		zap.String("encoder", l.encoder.element), zap.Bool("encodes", shape.encode), zap.Bool("pictures", shape.pixels))
	before := l.counts()

	l.tags.clear()
	l.discardQueuedFrames()
	started := l.sinceCreated()
	l.lastUsableAt.Store(started)
	l.feeding.Store(true)

	ended := make(chan error, 2+len(outputs))
	var running sync.WaitGroup
	running.Go(func() { ended <- l.feedChild(childCtx, stdinW) })
	running.Go(func() { ended <- l.watchChild(childCtx, started) })
	for _, output := range outputs {
		running.Go(func() { ended <- l.publishOutput(output) })
	}

	select {
	case <-ctx.Done():
	case stopped = <-ended:
	}
	l.feeding.Store(false)
	cancel()
	cmd.Process.Kill() //nolint:errcheck
	// Closing the parent's pipe ends unblocks a write to a child that stopped
	// reading and the reads of its outputs.
	closeOpened()
	running.Wait()
	waitErr := cmd.Wait()
	l.logger.Info("derived frame pipeline stopped", append(l.counts().since(before).fields(), zap.String("device", l.device))...)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return stopped, gstCameraPipelineError(l.device, l.elementInUse(),
		strings.TrimSpace(stderr.buf.String()), waitErr, status.Errorf(codes.Internal, "derived frame pipeline ended: %v", stopped))
}

func (l *derivedLayer) discardQueuedFrames() {
	for {
		select {
		case <-l.feed:
		default:
			return
		}
	}
}

// feedChild gives the child whole frames that hold a picture, from one decoding
// can start on, and waits for the next such frame after any frame it did not
// feed: one that never reached it, or one the driver flagged or cut short.
func (l *derivedLayer) feedChild(ctx context.Context, stdin *os.File) error {
	gdp := &gdpWriter{w: stdin}
	if err := gdp.streamHeader(l.transform.inputCaps(l.width, l.height, l.framerate)); err != nil {
		return fmt.Errorf("child stopped reading frames: %w", err)
	}
	started := time.Now()
	previous := uint64(0)
	var withoutPicture []byte
	awaitStart := func() {
		l.awaitingStart.Store(true)
		withoutPicture = nil
	}
	awaitStart()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame := <-l.feed:
			if sampleGap(previous, frame.sampleID) {
				awaitStart()
			}
			previous = frame.sampleID
			if !frame.auAligned || frame.nativeFlags&v4l2BufFlagError != 0 {
				l.damaged.Add(1)
				awaitStart()
				continue
			}
			whole := *frame
			if l.transform.holdsPicture != nil && !l.transform.holdsPicture(frame.data) {
				if len(withoutPicture)+len(frame.data) <= maxFrameBytes {
					withoutPicture = append(withoutPicture, frame.data...)
				}
				continue
			}
			if len(withoutPicture) > 0 && len(withoutPicture)+len(frame.data) <= maxFrameBytes {
				whole.data = append(withoutPicture, frame.data...)
			}
			withoutPicture = nil
			if l.awaitingStart.Load() {
				if _, startsHere := frameRandomAccess(&whole); !startsHere {
					l.awaitedStart.Add(1)
					continue
				}
				l.awaitingStart.Store(false)
			}
			if l.transform.reorders != nil && l.transform.reorders(whole.data) {
				return errDerivedFramesReordered
			}
			tag := l.tags.assign(frame, time.Since(started))
			if err := gdp.buffer(tag, whole.data); err != nil {
				return fmt.Errorf("child stopped reading frames: %w", err)
			}
			l.fed.Add(1)
		}
	}
}

// watchChild ends a child that capture keeps offering frames to and that has
// produced no usable output for the limit: it is silent, or, when it is fed
// nothing because no frame came that decoding can start on, the stream has no
// start. A camera that stopped delivering is the capture's to report, so a
// child nobody offers frames to is left alone.
func (l *derivedLayer) watchChild(ctx context.Context, started int64) error {
	check := time.NewTicker(max(min(l.firstOutputTimeout, l.idleOutputTimeout)/10, time.Millisecond))
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-check.C:
		}
		limit, lastUsable := l.idleOutputTimeout, l.lastUsableAt.Load()
		if lastUsable == started {
			limit = l.firstOutputTimeout
		}
		now, offered := l.sinceCreated(), l.lastOffered.Load()
		if offered > lastUsable && now-lastUsable > int64(limit) && now-offered < int64(limit) {
			if l.awaitingStart.Load() {
				return fmt.Errorf("%w for %s (%d frames passed over, %d flagged or cut short)",
					errDerivedNoStartFrame, limit, l.awaitedStart.Load(), l.damaged.Load())
			}
			return fmt.Errorf("%w for %s", errDerivedChildSilent, limit)
		}
	}
}

func (l *derivedLayer) publishOutput(output derivedOutput) error {
	gdp := &gdpReader{r: output.pipe, maxPayload: output.maxPayload}
	for {
		timestamp, payloadBytes, err := gdp.nextBufferHeader()
		if errors.Is(err, errGDPPayloadTooLarge) {
			l.unusable.Add(1)
			continue
		}
		if err != nil {
			return fmt.Errorf("%s output: %w", output.name, err)
		}
		source, known := l.tags.source(timestamp, gdp.segmentStart)
		if !known {
			continue
		}
		if !output.usable(payloadBytes) {
			l.unusable.Add(1)
			continue
		}
		l.usable.Add(1)
		l.lastUsableAt.Store(l.sinceCreated())
		if !output.wanted() {
			continue
		}
		payload, err := gdp.payload()
		if err != nil {
			return fmt.Errorf("%s output: %w", output.name, err)
		}
		if _, err := l.publish(source, output.frame(payload)); err != nil {
			l.unusable.Add(1)
			continue
		}
		l.published.Add(1)
	}
}

type derivedLayerCounts struct {
	fed, notFed, damaged, awaitedStart, unknownTags, unusable, usable, published, childRuns uint64
}

func (l *derivedLayer) counts() derivedLayerCounts {
	return derivedLayerCounts{
		fed: l.fed.Load(), notFed: l.notFed.Load(), damaged: l.damaged.Load(), awaitedStart: l.awaitedStart.Load(),
		unknownTags: l.tags.unknownOutputs(), unusable: l.unusable.Load(),
		usable: l.usable.Load(), published: l.published.Load(), childRuns: l.childRuns.Load(),
	}
}

func (c derivedLayerCounts) since(before derivedLayerCounts) derivedLayerCounts {
	return derivedLayerCounts{
		fed: c.fed - before.fed, notFed: c.notFed - before.notFed, damaged: c.damaged - before.damaged,
		awaitedStart: c.awaitedStart - before.awaitedStart,
		unknownTags:  c.unknownTags - before.unknownTags,
		unusable:     c.unusable - before.unusable, usable: c.usable - before.usable, published: c.published - before.published,
		childRuns: c.childRuns - before.childRuns,
	}
}

func (c derivedLayerCounts) fields() []zap.Field {
	return []zap.Field{
		zap.Uint64("frames_fed", c.fed), zap.Uint64("frames_not_fed", c.notFed),
		zap.Uint64("frames_flagged_or_cut_short", c.damaged),
		zap.Uint64("frames_passed_over_awaiting_a_start", c.awaitedStart),
		zap.Uint64("outputs_with_unknown_tag", c.unknownTags), zap.Uint64("outputs_unusable", c.unusable),
		zap.Uint64("outputs_usable", c.usable), zap.Uint64("frames_published", c.published), zap.Uint64("child_runs", c.childRuns),
	}
}
