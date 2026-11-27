package appservice

import (
	"context"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/cameraframe"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// GetSensorFrame snapshots the provider once. A faster camera cannot replace
// bytes midway through a transfer. The existing service identity, RPC slots,
// byte budget and timeout also apply to this stream.
func (r *rpcServer) GetSensorFrame(request *appv1.GetSensorFrameRequest, stream grpc.ServerStreamingServer[appv1.SensorFrameChunk]) error {
	if err := r.enter(stream.Context()); err != nil {
		return err
	}
	defer r.leave()
	s := r.service
	ctx, cancel := context.WithTimeout(stream.Context(), s.manager.cfg.Limits.RequestTimeout)
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	if request == nil || request.ComponentType == "" || len(request.ComponentType) > 256 || request.Stream == "" || len(request.Stream) > 256 || proto.Size(request) > s.manager.cfg.Limits.MaxRequestBytes {
		return status.Error(codes.InvalidArgument, "invalid camera frame request")
	}
	if !s.charge(proto.Size(request)) {
		return status.Error(codes.ResourceExhausted, "service camera budget exceeded")
	}
	fn := s.manager.cfg.GetSensorFrame
	if fn == nil {
		return status.Error(codes.Unimplemented, "native camera unavailable")
	}
	fence := &Fence{service: s, ctx: ctx}
	frame, err := fn(ctx, s.scope, serviceorigin.PublishScope{ComponentType: request.ComponentType, Stream: request.Stream}, fence)
	committed := fence.finish()
	if err != nil || !committed {
		return status.Error(codes.Unavailable, "native camera unavailable for service")
	}
	if frame.Validate() != nil {
		return status.Error(codes.Unavailable, "invalid native camera frame")
	}
	metadata, err := cameraframe.EncodeMetadata(frame.Metadata)
	if err != nil {
		return status.Error(codes.Unavailable, "invalid native camera metadata")
	}
	for offset := 0; offset < len(frame.JPEG); {
		end := min(offset+cameraframe.MaxChunkBytes, len(frame.JPEG))
		chunk := &appv1.SensorFrameChunk{MetadataJson: metadata, Offset: int64(offset), Data: frame.JPEG[offset:end]}
		if err := s.authorize(ctx); err != nil {
			return status.Error(codes.PermissionDenied, "service authority changed")
		}
		if proto.Size(chunk) > s.manager.cfg.Limits.MaxResponseBytes || !s.charge(proto.Size(chunk)) {
			return status.Error(codes.ResourceExhausted, "service camera response budget exceeded")
		}
		if finalize := s.manager.cfg.FinalizeResponse; finalize != nil {
			out, err := finalize(ctx, s.scope, chunk)
			if err != nil || out != chunk {
				return status.Error(codes.Unavailable, "camera response no longer available")
			}
		}
		if err := s.responseAlive(ctx); err != nil {
			return status.Error(codes.PermissionDenied, "original service peer is no longer available")
		}
		if err := sendSensorChunk(ctx, stream, chunk); err != nil {
			return err
		}
		offset = end
	}
	return nil
}

func sendSensorChunk(ctx context.Context, stream grpc.ServerStreamingServer[appv1.SensorFrameChunk], chunk *appv1.SensorFrameChunk) error {
	sent := make(chan error, 1)
	go func() { sent <- stream.Send(chunk) }()
	select {
	case err := <-sent:
		return err
	case <-ctx.Done():
		closeContextConnection(stream.Context())
		<-sent
		return status.Error(codes.DeadlineExceeded, "camera response deadline")
	}
}
