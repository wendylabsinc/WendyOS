package appservice

import (
	"context"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Only generated methods have a profiling name. Caller-controlled method text,
// metadata, requests, response bytes and error descriptions are never recorded.
func traceMethod(full string) (rpctrace.Method, bool) {
	switch full {
	case appv1.WorldAppService_Publish_FullMethodName:
		return rpctrace.MethodPublish, true
	case appv1.WorldAppService_PublishStream_FullMethodName:
		return rpctrace.MethodPublishStream, true
	case appv1.WorldAppService_Query_FullMethodName:
		return rpctrace.MethodQuery, true
	case appv1.WorldAppService_GetEvidence_FullMethodName:
		return rpctrace.MethodGetEvidence, true
	case appv1.WorldAppService_PutEvidence_FullMethodName:
		return rpctrace.MethodPutEvidence, true
	case appv1.WorldAppService_GetRecordProof_FullMethodName:
		return rpctrace.MethodGetRecordProof, true
	case appv1.WorldAppService_Clock_FullMethodName:
		return rpctrace.MethodClock, true
	case appv1.WorldAppService_PhysicalClock_FullMethodName:
		return rpctrace.MethodPhysicalClock, true
	case appv1.WorldAppService_GetSourceAuthority_FullMethodName:
		return rpctrace.MethodGetSourceAuthority, true
	default:
		return 0, false
	}
}

func (s *service) traceContext(ctx context.Context, method rpctrace.Method) (context.Context, func(rpctrace.Code)) {
	return rpctrace.Begin(ctx, s.manager.cfg.Trace, rpctrace.Labels{Method: method, App: s.scope.App, Service: s.scope.Service, Generation: s.scope.DeploymentGeneration})
}

// Interceptors finish after the handler's cancellation, admission and RPC-slot
// defers. Finishing only offers an owned event to a bounded nonblocking sink;
// bounded event encoding happens after those releases. Writer I/O belongs to
// the host's separate Sink.Run loop.
func (s *service) traceUnary(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (reply any, err error) {
	method, ok := traceMethod(info.FullMethod)
	if !ok {
		return handler(ctx, request)
	}
	ctx, finish := s.traceContext(ctx, method)
	code := codes.Unknown
	defer func() { finish(rpctrace.Code(code)) }()
	reply, err = handler(ctx, request)
	code = status.Code(err)
	return reply, err
}

type traceServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s traceServerStream) Context() context.Context { return s.ctx }

// Streaming RPCs produce one bounded aggregate when the stream terminates.
// Receive/send behavior and per-message authorization are unchanged.
func (s *service) traceStream(server any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	method, ok := traceMethod(info.FullMethod)
	if !ok {
		return handler(server, stream)
	}
	ctx, finish := s.traceContext(stream.Context(), method)
	code := codes.Unknown
	defer func() { finish(rpctrace.Code(code)) }()
	err = handler(server, traceServerStream{stream, ctx})
	code = status.Code(err)
	return err
}
