package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// pipelineShape chooses which branches planGStreamerPipelineShaped builds.
type pipelineShape struct {
	// encode adds the H.264 branch writing fd 1. False builds only the tap on
	// rawTapFD, so a camera nobody is watching costs no encode.
	encode bool
	// pixels adds a branch of decoded pictures. Only a derived layer's pipeline
	// reads it: a camera pipeline tees its tap whenever it has one.
	pixels bool
	// preset is what the encode branch's encoder runs at. Unset without that
	// branch and for an encoder that takes none, so that a preset never tells
	// two shapes apart unless both run an encoder it changes.
	preset appconfig.EncodePreset
}

func (s pipelineShape) lacksBranchOf(wanted pipelineShape) bool {
	return (wanted.encode && !s.encode) || (wanted.pixels && !s.pixels)
}

func (s pipelineShape) hasBranchNotIn(wanted pipelineShape) bool {
	return (s.encode && !wanted.encode) || (s.pixels && !wanted.pixels)
}

func (s pipelineShape) encodesSlowerThan(wanted pipelineShape) bool {
	return wanted.preset.FasterThan(s.preset)
}

func (s pipelineShape) encodesFasterThan(wanted pipelineShape) bool {
	return s.preset.FasterThan(wanted.preset)
}

var errPipelineEndedRestartable = errors.New("pipeline ended and can be restarted")

type shapeNeeds func(demand encodeDemand) pipelineShape

func encoderOnDemand(demand encodeDemand) pipelineShape {
	return pipelineShape{encode: demand.wantEncoded()}
}

// A pipeline that encodes keeps its pictures, as a camera pipeline keeps its
// tap, so a pixel subscriber joining or leaving never restarts it under a
// viewer or a recording.
func branchesOnDemand(demand encodeDemand) pipelineShape {
	encode := demand.wantEncoded()
	return pipelineShape{encode: encode, pixels: encode || demand.wantRaw()}
}

// errNoTapOnlyPipeline is returned when a shape without the encoder is asked
// of a pipeline that has no uncompressed frames to hand out.
var errNoTapOnlyPipeline = errors.New("pipeline has no uncompressed frames to run without an encoder")

// yuyvPictureStages converts decoded frames to YUYV. A decoder's own output
// format varies (I420 and others); pinning YUY2 makes the bytes match the
// RawFormat yuyvPictureFormat reports.
const yuyvPictureStages = "videoconvert ! video/x-raw,format=YUY2"

var mjpegTapTail = fmt.Sprintf("%s ! fdsink fd=%d", yuyvPictureStages, rawTapFD)

func mjpegRawWhy(w, h uint32) string {
	return fmt.Sprintf("camera is captured as MJPEG at %dx%d; raw frames are not offered", w, h)
}

// yuyvPictureFormat describes the YUYV pictures decoded from a camera's
// compressed frames, or nil when one would exceed the raw frame limit.
func yuyvPictureFormat(w, h uint32) *agentpb.RawFormat {
	if uint64(w)*2*uint64(h) > maxRawFrameBytes {
		return nil
	}
	return &agentpb.RawFormat{Width: w, Height: h, Fourcc: fourccYUYV, BytesPerLine: w * 2}
}

// tapFormat is the format of the frames the pipeline writes to rawTapFD, or nil
// when it writes none. runCameraPipeline opens and drains the pipe whenever this
// is non-nil: a pipeline whose fd 3 has no reader or no descriptor would fail.
// The planners set pixels wherever they set raw; falling back to raw keeps fd 3
// open for a plan that has a tap of either kind.
func tapFormat(plan gstPipelinePlan) *agentpb.RawFormat {
	if plan.pixels != nil {
		return plan.pixels
	}
	return plan.raw
}

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
// waiting for its first frame. It is dropped only after grace with no viewer or
// recording among the subscribers that remain. With nobody at all the hub
// cancels ctx, so there is nothing to switch to.
//
// encoder is the element the encode branch runs. It runs at the fastest preset
// a subscriber to encoded frames states, so that it never costs more than the
// cheapest request on the camera allows: a pipeline that encodes is launched
// again at once for a faster preset than it runs at, and for a slower one only
// after grace, like a branch nobody wants any more. An encoder that takes no
// preset is never launched again for one.
//
// launch runs one pipeline until its ctx ends or the pipeline does. Each launch
// returns before the next starts, so the device is released in between. A
// launch without the encoder that returns errNoTapOnlyPipeline says this camera
// has no frames to run without one, so the encoder then stays for the rest of
// this call. Any other end of a launch this loop did not cancel ends the loop
// with that launch's error, as a single pipeline's end did before.
//
// With no demand to read (a producer whose sink is not a hub), launch runs once
// with the encoder, at the default preset.
func runShapedPipelines(ctx context.Context, demand encodeDemand, grace time.Duration, encoder string, launch func(ctx context.Context, shape pipelineShape) error) error {
	return runPipelinesShapedBy(ctx, demand, grace, encoderOnDemand, encoder, launch)
}

func runPipelinesShapedBy(ctx context.Context, demand encodeDemand, grace time.Duration, needs shapeNeeds, encoder string, launch func(ctx context.Context, shape pipelineShape) error) error {
	if demand == nil {
		return launch(ctx, pipelineShape{encode: true})
	}
	noTap := false
	// wanted is the shape the subscribers call for now, and the only place it is
	// decided: each launch starts in it, and a running launch is replaced only
	// when it differs, never by another launch of the same shape.
	wanted := func() pipelineShape {
		shape := needs(demand)
		shape.encode = shape.encode || noTap
		if shape.encode {
			shape.preset = encoderPreset(encoder, demand.statedPreset())
		}
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
		demand.setEncoderPreset(shape.preset)
		switched, err := superviseShape(ctx, demand, grace, shape, wanted, launch)
		if switched {
			continue
		}
		if !shape.encode && errors.Is(err, errNoTapOnlyPipeline) {
			noTap = true
			continue
		}
		if errors.Is(err, errPipelineEndedRestartable) && ctx.Err() == nil {
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
	// branchIdle is the one condition for dropping a branch, the encoder or a
	// derived layer's pictures, and for slowing the encoder down: the hub still
	// has an audience, and the shape it calls for lacks a branch this launch
	// runs, or encodes at a slower preset.
	branchIdle := func() bool {
		wanted := wanted()
		return (shape.hasBranchNotIn(wanted) || shape.encodesFasterThan(wanted)) && (demand.wantEncoded() || demand.wantRaw() || demand.wantNative())
	}

	for {
		// Re-read the hub on every pass, starting with the first: a change that
		// landed between the shape decision and this launch is acted on too.
		switch wanted := wanted(); {
		case shape.lacksBranchOf(wanted) || shape.encodesSlowerThan(wanted):
			stop()
			return true, nil
		case branchIdle():
			// Started once and left running while the branch stays idle, so a run
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
			//
			// One arriving after this check, before stop has killed the
			// pipeline, may be handed the last of the old pipeline's output and
			// then the new pipeline's stream from its start (the loop relaunches
			// with the encoder for it). The window is the few milliseconds the
			// kill takes, and the new stream opens with a keyframe the viewer's
			// decoder can start from, so it is accepted rather than closed by
			// holding the hub's lock across the kill.
			if branchIdle() {
				stop()
				return true, nil
			}
		}
	}
}
