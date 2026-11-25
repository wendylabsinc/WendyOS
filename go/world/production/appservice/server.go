package appservice

import (
	"context"
	"errors"
	"io"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type rpcServer struct {
	appv1.UnimplementedWorldAppServiceServer
	service *service
}

func newServer(s *service) *grpc.Server {
	l := s.manager.cfg.Limits
	options := []grpc.ServerOption{grpc.Creds(&localCredentials{service: s}), grpc.MaxRecvMsgSize(l.MaxRequestBytes), grpc.MaxSendMsgSize(l.MaxResponseBytes), grpc.MaxConcurrentStreams(uint32(l.MaxConcurrentRPCs)), grpc.MaxHeaderListSize(16 << 10), grpc.ConnectionTimeout(l.RequestTimeout)}
	if s.manager.cfg.Trace != nil {
		options = append(options, grpc.UnaryInterceptor(s.traceUnary), grpc.StreamInterceptor(s.traceStream))
	}
	server := grpc.NewServer(options...)
	appv1.RegisterWorldAppServiceServer(server, &rpcServer{service: s})
	return server
}

func (r *rpcServer) enter(ctx context.Context) error {
	if err := r.service.authorize(ctx); err != nil {
		return status.Error(codes.PermissionDenied, "service authority denied")
	}
	select {
	case r.service.rpcs <- struct{}{}:
		return nil
	default:
		return status.Error(codes.ResourceExhausted, "service RPC budget exceeded")
	}
}
func (r *rpcServer) leave() { <-r.service.rpcs }

func (r *rpcServer) Publish(ctx context.Context, request *appv1.PublishRequest) (*appv1.PublishReply, error) {
	if err := r.enter(ctx); err != nil {
		return nil, err
	}
	defer r.leave()
	return r.publish(ctx, request)
}

func (r *rpcServer) PublishStream(stream grpc.BidiStreamingServer[appv1.PublishRequest, appv1.PublishReply]) error {
	if err := r.enter(stream.Context()); err != nil {
		return err
	}
	defer r.leave()
	for {
		// Check before waiting as well as after receipt. Host invalidation
		// closes the actual connection, including an idle stream.
		if err := r.service.authorize(stream.Context()); err != nil {
			return status.Error(codes.PermissionDenied, "service authority denied")
		}
		request, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		reply, err := r.publish(stream.Context(), request)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(stream.Context(), r.service.manager.cfg.Limits.RequestTimeout)
		if err = r.service.authorize(ctx); err != nil {
			cancel()
			return status.Error(codes.PermissionDenied, "service authority changed; publication outcome may be unknown")
		}
		err = sendReply(ctx, stream, reply)
		cancel()
		if err != nil {
			return err
		}
	}
}

// The caller owns one of the service's bounded RPC slots. At most one send is
// outstanding per slot, and cancellation closes its transport before returning.
func sendReply(ctx context.Context, stream grpc.BidiStreamingServer[appv1.PublishRequest, appv1.PublishReply], reply *appv1.PublishReply) error {
	sent := make(chan error, 1)
	go func() { sent <- stream.Send(reply) }()
	select {
	case err := <-sent:
		return err
	case <-ctx.Done():
		closeContextConnection(stream.Context())
		<-sent
		return status.Error(codes.DeadlineExceeded, "publication response deadline; outcome may be unknown")
	}
}

func closeContextConnection(ctx context.Context) {
	if p, ok := peer.FromContext(ctx); ok {
		if a, ok := p.AuthInfo.(serviceAuthInfo); ok && a.conn != nil {
			_ = a.conn.Close()
		}
	}
}

func (r *rpcServer) publish(parent context.Context, request *appv1.PublishRequest) (*appv1.PublishReply, error) {
	s := r.service
	ctx, cancel := context.WithTimeout(parent, s.manager.cfg.Limits.RequestTimeout)
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := s.authorize(ctx); err != nil {
		return nil, status.Error(codes.PermissionDenied, "service authority denied")
	}
	if request == nil || proto.Size(request) > s.manager.cfg.Limits.MaxRequestBytes {
		return nil, status.Error(codes.InvalidArgument, "publication request exceeds budget")
	}
	if !s.charge(proto.Size(request)) {
		return nil, status.Error(codes.ResourceExhausted, "service publication budget exceeded")
	}
	publication, err := appv1.DecodePublish(request)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid structured publication")
	}
	publishing := rpctrace.Start(ctx, rpctrace.StagePublisher)
	record, err := s.publisher.Publish(ctx, publication)
	publishing()
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "publication authority denied")
	}
	reply, err := appv1.EncodePublishReply(record)
	if err != nil || reply == nil {
		return nil, status.Error(codes.ResourceExhausted, "publication response exceeds budget")
	}
	// Include the acceptance bit in the reserved response size. The reply is
	// not returned to the transport until the sink and final checks succeed.
	reply.Accepted = true
	if proto.Size(reply) > s.manager.cfg.Limits.MaxResponseBytes {
		return nil, status.Error(codes.ResourceExhausted, "publication response exceeds budget")
	}
	// Reserve response capacity before admission, avoiding an avoidable
	// unknown outcome caused by our own output limiter after a successful sink.
	if !s.charge(proto.Size(reply)) {
		return nil, status.Error(codes.ResourceExhausted, "service response budget exceeded")
	}
	if err = s.authorize(ctx); err != nil {
		return nil, status.Error(codes.PermissionDenied, "service authority changed before admission")
	}
	fence := &Fence{service: s, ctx: ctx}
	err = s.manager.cfg.Accept(ctx, s.scope, record, fence)
	committed := fence.finish()
	if err != nil || !committed {
		return nil, status.Error(codes.Unavailable, ErrAdmission.Error())
	}
	if err = s.authorize(ctx); err != nil {
		return nil, status.Error(codes.Unavailable, "service authority changed after admission; publication outcome may be unknown")
	}
	reply.Accepted = true
	return reply, nil
}

func (s *service) charge(size int) bool {
	// Wall/monotonic time here measures only resource budgets. It is never
	// accepted as signing, source freshness, or entitlement authority time.
	now := time.Now()
	return size >= 0 && s.messages.AllowN(now, 1) && s.bytes.AllowN(now, size)
}
