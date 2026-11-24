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
	decoderElements []string
	inputCaps       func(width, height, framerate uint32) string
	decodeStages    string
	pictureStages   string
	pictures        func(width, height uint32) *agentpb.RawFormat
	rawWhy          func(width, height uint32) string
}

var derivedTransforms = map[agentpb.VideoCodec]derivedTransform{
	videoCodecMJPEG: {
		decoderElements: []string{"jpegdec"},
		inputCaps: func(width, height, framerate uint32) string {
			return fmt.Sprintf("image/jpeg, width=(int)%d, height=(int)%d, framerate=(fraction)%d/1", width, height, framerate)
		},
		decodeStages:  "jpegdec",
		pictureStages: mjpegPixelsStages,
		pictures:      mjpegPixelsFormat,
		rawWhy:        mjpegRawWhy,
	},
}

var derivedFramingElements = []string{"gdpdepay", "gdppay", "h264parse"}

func derivedLayerMissingElements(transform derivedTransform, available map[string]bool) []string {
	var missing []string
	for _, element := range append(append([]string(nil), derivedFramingElements...), transform.decoderElements...) {
		if !available[element] {
			missing = append(missing, element)
		}
	}
	sort.Strings(missing)
	return missing
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

// derivedLayer turns the native frames a hub numbers into decoded pictures and
// H.264 with one child pipeline, which runs only in the shape, and only while,
// the hub's subscribers call for. Each frame is fed with a tag for a timestamp
// and each output is published under the identity of the frame its tag names.
type derivedLayer struct {
	logger    *zap.Logger
	device    string
	format    agentpb.VideoCodec
	transform derivedTransform
	gstPath   string
	encoder   gstEncoderResult
	grace     time.Duration

	width, height, framerate uint32
	pictures                 *agentpb.RawFormat

	demand  encodeDemand
	publish func(source, derived *videoFrame) (keepProducing bool, err error)

	feed    chan *videoFrame
	feeding atomic.Bool
	tags    derivedFrameTags

	notFed, unusable     atomic.Uint64
	published, childRuns atomic.Uint64
	failedStarts         int

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

// newDerivedLayer returns the layer, or why this device cannot run one.
func newDerivedLayer(cfg derivedLayerConfig) (*derivedLayer, error) {
	transform, known := derivedTransforms[cfg.format]
	if !known {
		return nil, fmt.Errorf("no derived frames are defined for %s", codecIndexName(cfg.format))
	}
	if missing := derivedLayerMissingElements(transform, cfg.available); len(missing) > 0 {
		return nil, fmt.Errorf("GStreamer lacks %s", strings.Join(missing, ", "))
	}
	enc, err := derivedLayerEncoder(cfg.available)
	if err != nil {
		return nil, err
	}
	framerate := cfg.framerate
	if framerate == 0 {
		framerate = 30
	}
	return &derivedLayer{
		logger: cfg.logger, device: cfg.device, format: cfg.format, transform: transform,
		gstPath: cfg.gstPath, encoder: enc, grace: cfg.grace,
		width: cfg.width, height: cfg.height, framerate: framerate,
		pictures: transform.pictures(cfg.width, cfg.height),
		demand:   cfg.demand, publish: cfg.publish,
		feed:               make(chan *videoFrame, derivedFeedQueueFrames),
		firstOutputTimeout: firstFrameTimeout, idleOutputTimeout: cameraIdleTimeout,
		restartPause: derivedRestartPause,
	}, nil
}

// take is the layer's only intake, and the hub's produce its only caller. It
// never blocks the capture that calls it: a frame the child has no room for is
// not fed, and counted.
func (l *derivedLayer) take(frame *videoFrame) {
	if frame.codec != l.format || frame.sampleID == 0 || !l.feeding.Load() {
		return
	}
	select {
	case l.feed <- frame:
	default:
		l.notFed.Add(1)
	}
}

// offerTo makes the raw and pixel decision for the capture this layer serves.
func (l *derivedLayer) offerTo(sink rawSink) {
	if pixels, ok := sink.(pixelSink); ok && l.pictures != nil {
		pixels.pixelsOffered(l.pictures)
	}
	sink.rawNotOffered(fixedReason(l.transform.rawWhy(l.width, l.height)))
}

func (l *derivedLayer) needs(demand encodeDemand) pipelineShape {
	shape := branchesOnDemand(demand)
	shape.pixels = shape.pixels && l.pictures != nil
	return shape
}

func (l *derivedLayer) run(ctx context.Context) error {
	return runPipelinesShapedBy(ctx, l.demand, l.grace, l.needs, l.launch)
}

func (l *derivedLayer) pipelineArgs(shape pipelineShape) []string {
	decode := "fdsrc fd=0 ! gdpdepay ! " + l.transform.decodeStages
	pictures := fmt.Sprintf("%s ! gdppay ! fdsink fd=%d", l.transform.pictureStages, rawTapFD)
	encoded := encoderSegment(l.encoder.element, l.encoder.hasH264Parse, keyframeIntervalFrames(l.framerate)) + " ! gdppay ! fdsink fd=1"
	var pipeline string
	switch {
	case shape.encode && shape.pixels:
		pipeline = fmt.Sprintf("%s ! tee name=%s %s. ! queue ! %s %s. ! queue ! %s",
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
	publishedBefore := l.published.Load()
	err := l.runChild(ctx, shape)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if l.published.Load() > publishedBefore {
		l.failedStarts = 0
	} else if l.failedStarts++; l.failedStarts >= derivedFailedStartsBeforeGivingUp {
		return status.Errorf(codes.Internal, "derived frame pipeline produced nothing in %d starts: %s", l.failedStarts, status.Convert(err).Message())
	}
	l.logger.Warn("derived frame pipeline ended; restarting it", zap.String("device", l.device),
		zap.Bool("encodes", shape.encode), zap.Bool("pictures", shape.pixels), zap.Error(err))
	if l.failedStarts > 0 {
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
	frame      func(payload []byte) *videoFrame
}

func (l *derivedLayer) pictureFrame(payload []byte) *videoFrame {
	if len(payload) != int(l.pictures.GetBytesPerLine())*int(l.pictures.GetHeight()) {
		return nil
	}
	return &videoFrame{data: payload, codec: agentpb.VideoCodec_VIDEO_CODEC_RAW, rawFmt: l.pictures}
}

func accessUnitFrame(payload []byte) *videoFrame {
	if len(payload) == 0 {
		return nil
	}
	return &videoFrame{data: payload, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: true}
}

// runChild runs one child pipeline in shape until ctx ends or the child stops
// taking or giving frames, and returns why it stopped.
func (l *derivedLayer) runChild(ctx context.Context, shape pipelineShape) (runErr error) {
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := l.pipelineArgs(shape)
	cmd := exec.CommandContext(childCtx, args[0], args[1:]...)
	oomscore.ResetChild(cmd)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C")
	stderr := &limitedBuffer{limit: maxStderrBytes}
	cmd.Stderr = stderr

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
		return status.Errorf(codes.Internal, "failed to create derived frame pipe: %v", err)
	}
	cmd.Stdin = stdinR
	childEnds := []*os.File{stdinR}
	var outputs []derivedOutput
	if shape.encode {
		r, w, err := pipe()
		if err != nil {
			closeOpened()
			return status.Errorf(codes.Internal, "failed to create derived frame pipe: %v", err)
		}
		cmd.Stdout = w
		childEnds = append(childEnds, w)
		outputs = append(outputs, derivedOutput{name: "H.264", pipe: r, maxPayload: maxFrameBytes, frame: accessUnitFrame})
	}
	if shape.pixels {
		r, w, err := pipe()
		if err != nil {
			closeOpened()
			return status.Errorf(codes.Internal, "failed to create derived frame pipe: %v", err)
		}
		// The first ExtraFiles slot is the child's fd 3, rawTapFD.
		cmd.ExtraFiles = []*os.File{w}
		childEnds = append(childEnds, w)
		outputs = append(outputs, derivedOutput{name: "pictures", pipe: r, maxPayload: maxRawFrameBytes, frame: l.pictureFrame})
	}
	if err := cmd.Start(); err != nil {
		closeOpened()
		return status.Errorf(codes.Internal, "failed to start derived frame pipeline: %v", err)
	}
	for _, f := range childEnds {
		f.Close() //nolint:errcheck
	}
	l.childRuns.Add(1)
	l.logger.Info("derived frame pipeline started", zap.String("device", l.device),
		zap.String("encoder", l.encoder.element), zap.Bool("encodes", shape.encode), zap.Bool("pictures", shape.pixels))

	l.tags.clear()
	l.discardQueuedFrames()
	l.feeding.Store(true)

	ended := make(chan error, 1+len(outputs))
	var running sync.WaitGroup
	running.Go(func() { ended <- l.feedChild(childCtx, stdinW) })
	for _, output := range outputs {
		running.Go(func() { ended <- l.publishOutput(output) })
	}

	var stopped error
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
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(stopped, os.ErrDeadlineExceeded) {
		stopped = status.Error(codes.DeadlineExceeded, "derived frame pipeline stopped producing frames")
	} else {
		stopped = status.Errorf(codes.Internal, "derived frame pipeline ended: %v", stopped)
	}
	return gstCameraPipelineError(l.device, l.encoder.element, strings.TrimSpace(stderr.buf.String()), waitErr, stopped)
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

func (l *derivedLayer) feedChild(ctx context.Context, stdin *os.File) error {
	gdp := &gdpWriter{w: stdin}
	if err := gdp.streamHeader(l.transform.inputCaps(l.width, l.height, l.framerate)); err != nil {
		return fmt.Errorf("child stopped reading frames: %w", err)
	}
	started := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame := <-l.feed:
			tag := l.tags.assign(frame, time.Since(started))
			if err := gdp.buffer(tag, frame.data); err != nil {
				return fmt.Errorf("child stopped reading frames: %w", err)
			}
		}
	}
}

func (l *derivedLayer) publishOutput(output derivedOutput) error {
	gdp := &gdpReader{
		r:          &cameraPipeReader{pipe: output.pipe, timeout: l.firstOutputTimeout, idleTimeout: l.idleOutputTimeout},
		maxPayload: output.maxPayload,
	}
	for {
		timestamp, payload, err := gdp.nextBuffer()
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
		derived := output.frame(payload)
		if derived == nil {
			l.unusable.Add(1)
			continue
		}
		if _, err := l.publish(source, derived); err != nil {
			l.unusable.Add(1)
			continue
		}
		l.published.Add(1)
	}
}

type derivedLayerCounts struct {
	notFed, unknownTags, unusable, published, childRuns uint64
}

func (l *derivedLayer) counts() derivedLayerCounts {
	return derivedLayerCounts{
		notFed: l.notFed.Load(), unknownTags: l.tags.unknownOutputs(), unusable: l.unusable.Load(),
		published: l.published.Load(), childRuns: l.childRuns.Load(),
	}
}
