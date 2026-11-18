package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// pipelineShape chooses which branches planGStreamerPipelineShaped builds.
type pipelineShape struct {
	// encode adds the H.264 branch writing fd 1. False builds only the tap on
	// rawTapFD, so a camera nobody is watching costs no encode.
	encode bool
}

// errNoTapOnlyPipeline is returned when a shape without the encoder is asked
// of a pipeline that has no uncompressed frames to hand out.
var errNoTapOnlyPipeline = errors.New("pipeline has no uncompressed frames to run without an encoder")

// mjpegTapTail converts decoded MJPEG frames to YUYV and writes them to the tap.
// jpegdec's native output varies (I420 and others); pinning YUY2 makes the bytes
// match the RawFormat the plan reports.
var mjpegTapTail = fmt.Sprintf("videoconvert ! video/x-raw,format=YUY2 ! fdsink fd=%d", rawTapFD)

// mjpegPixelsFormat describes the YUYV frames an MJPEG capture tees to the tap,
// or nil when a frame would exceed the raw frame limit.
func mjpegPixelsFormat(w, h uint32) *agentpb.RawFormat {
	if uint64(w)*2*uint64(h) > maxRawFrameBytes {
		return nil
	}
	return &agentpb.RawFormat{Width: w, Height: h, Fourcc: fourccYUYV, BytesPerLine: w * 2}
}

// tapFormat is the format of the frames the pipeline writes to rawTapFD, or nil
// when it writes none. runCameraPipeline opens and drains the pipe whenever this
// is non-nil: a pipeline whose fd 3 has no reader or no descriptor would fail.
func tapFormat(plan gstPipelinePlan) *agentpb.RawFormat { return plan.pixels }

// defaultEncoderIdleGrace is how long a pipeline keeps its encoder after the
// last viewer or recording leaves while apps still read its frames. Dropping the
// encoder restarts the pipeline, which pauses those apps for about a second, so
// a viewer that leaves and comes back within it must not cost two restarts.
const defaultEncoderIdleGrace = 30 * time.Second

// runShapedPipelines runs a camera's pipeline in the shape its subscribers need,
// and relaunches it when that need changes. The relaunch happens inside the same
// hub, so every subscriber stays attached across it.
//
// The encoder is added as soon as a viewer or recording arrives, because it is
// waiting for its first frame. It is dropped only after grace with raw or pixel
// subscribers alone. With nobody at all the hub cancels ctx, so there is nothing
// to switch to.
//
// launch runs one pipeline until its ctx ends or the pipeline does. Each launch
// returns before the next starts, so the device is released in between. A
// launch without the encoder that returns errNoTapOnlyPipeline says this camera
// has no frames to run without one, so the encoder then stays for the rest of
// this call. Any other end of a launch this loop did not cancel ends the loop
// with that launch's error, as a single pipeline's end did before.
//
// With no demand to read (a producer whose sink is not a hub), launch runs once
// with the encoder: today's behaviour.
func runShapedPipelines(ctx context.Context, demand encodeDemand, grace time.Duration, launch func(ctx context.Context, shape pipelineShape) error) error {
	if demand == nil {
		return launch(ctx, pipelineShape{encode: true})
	}
	noTap := false
	// wanted is the shape the subscribers call for now, and the only place it is
	// decided: each launch starts in it, and a running launch is replaced only
	// when it differs, never by another launch of the same shape.
	wanted := func() pipelineShape {
		shape := pipelineShape{encode: demand.wantEncoded()}
		shape.encode = shape.encode || noTap
		return shape
	}
	for launches := 0; ; launches++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		shape := wanted()
		if launches > 0 {
			// The new pipeline starts a new WebM stream; a VP8 viewer joining it
			// must be handed that stream's initialization, not the old one's.
			demand.encodedStreamRestarted()
		}
		demand.setPipelineEncodes(shape.encode)
		switched, err := superviseShape(ctx, demand, grace, shape, wanted, launch)
		if switched {
			continue
		}
		if !shape.encode && errors.Is(err, errNoTapOnlyPipeline) {
			noTap = true
			continue
		}
		return err
	}
}

// superviseShape runs one launch of shape and watches demand while it runs. It
// reports switched once it has cancelled the launch, and the launch has
// returned, because wanted now gives the other shape. Otherwise it returns the
// launch's own result, or ctx's error when the parent was cancelled.
func superviseShape(ctx context.Context, demand encodeDemand, grace time.Duration, shape pipelineShape, wanted func() pipelineShape, launch func(ctx context.Context, shape pipelineShape) error) (switched bool, err error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- launch(runCtx, shape) }()
	stop := func() {
		cancel()
		<-done
	}

	changed := demand.demandChanged()
	var graceTimer *time.Timer
	var graceUp <-chan time.Time
	stopGrace := func() {
		if graceTimer != nil {
			graceTimer.Stop()
			graceTimer, graceUp = nil, nil
		}
	}
	defer stopGrace()
	// encoderIdle is the one condition for dropping the encoder: apps still read
	// frames, and the shape they call for has none.
	encoderIdle := func() bool { return !wanted().encode && demand.wantRaw() }

	for {
		// Re-read the hub on every pass, starting with the first: a change that
		// landed between the shape decision and this launch is acted on too.
		switch {
		case !shape.encode && wanted().encode:
			stop()
			return true, nil
		case shape.encode && encoderIdle():
			// Started once and left running while the encoder stays idle, so a run
			// of raw joins and leaves cannot postpone the drop indefinitely.
			if graceTimer == nil {
				graceTimer = time.NewTimer(grace)
				graceUp = graceTimer.C
			}
		default:
			stopGrace()
		}
		select {
		case launchErr := <-done:
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, launchErr
		case <-ctx.Done():
			stop()
			return false, ctx.Err()
		case <-changed:
		case <-graceUp:
			graceTimer, graceUp = nil, nil
			// A viewer may have arrived as the timer fired; the next pass then
			// keeps the encoder instead.
			if encoderIdle() {
				stop()
				return true, nil
			}
		}
	}
}
