package services

import (
	"errors"
	"fmt"

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
