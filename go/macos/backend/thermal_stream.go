package backend

import (
	"context"
	"errors"
	"io"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type thermalVideoClient interface {
	StreamVideo(context.Context, *pb.StreamVideoRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.VideoFrame], error)
}

// Retry the subscription, not capture. This also serves inline viewers which
// do not own the camera wall's retry loop. Cancellation belongs to the same
// camera generation throughout, so pausing or replacing it stops every retry.
func (b *Backend) runThermalCamera(ctx context.Context, client thermalVideoClient, req *pb.StreamVideoRequest, profile *pb.ThermalCamera, session, streamID uint64, timeout time.Duration) {
	var summary thermalSummaryTracker
	failures := 0
	for ctx.Err() == nil {
		sawFrame, err := b.receiveThermalCamera(ctx, client, req, profile, session, streamID, timeout, &summary)
		if ctx.Err() != nil {
			return
		}
		if sawFrame {
			failures = 0
		}
		failures++
		if !retryThermalStream(err) || failures > 3 {
			b.emitText(ctx, EventCameraError, session, streamID, err.Error())
			return
		}
		timer := time.NewTimer(time.Second * time.Duration(1<<(failures-1)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func retryThermalStream(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Internal:
		return true
	default:
		return false
	}
}

func (b *Backend) receiveThermalCamera(parent context.Context, client thermalVideoClient, req *pb.StreamVideoRequest, profile *pb.ThermalCamera, session, streamID uint64, timeout time.Duration, summary *thermalSummaryTracker) (bool, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	type result struct {
		frame *pb.VideoFrame
		err   error
	}
	frames := make(chan result, 1)
	go func() {
		stream, err := client.StreamVideo(ctx, req)
		for {
			var frame *pb.VideoFrame
			if err == nil {
				frame, err = stream.Recv()
			}
			select {
			case frames <- result{frame, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	// Keep stale measurements marked stale in the UI while allowing a brief
	// cloud outage. The receive loop owns the timer, avoiding callback/reset races.
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	sawFrame := false
	for {
		select {
		case <-ctx.Done():
			return sawFrame, ctx.Err()
		case <-timer.C:
			return sawFrame, status.Error(codes.DeadlineExceeded, "thermal stream stalled; reconnecting")
		case r := <-frames:
			if r.err != nil {
				return sawFrame, r.err
			}
			if len(r.frame.GetData()) == 0 {
				continue
			}
			measured, err := decodeThermalFrame(r.frame, profile)
			if err != nil {
				return sawFrame, err
			}
			sawFrame = true
			summary.apply(measured, time.Now())
			timer.Reset(timeout)
			b.emit(ctx, Event{Kind: EventThermal, Session: session, Stream: streamID, Bytes: measured.RGBA, Thermal: measured})
		}
	}
}
