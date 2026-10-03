package mtls

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func (c *revocationChecker) checkRPC(ctx context.Context) error {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing TLS peer")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing TLS peer")
	}
	if err := c.check(ctx, info.State); err != nil {
		return status.Error(codes.Unauthenticated, "certificate revocation status does not permit access")
	}
	return nil
}
func (c *revocationChecker) unaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := c.checkRPC(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

type revocationStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *revocationStream) Context() context.Context { return s.ctx }
func (s *revocationStream) SendMsg(m any) error {
	if err := context.Cause(s.ctx); err != nil {
		return err
	}
	return s.ServerStream.SendMsg(m)
}
func (s *revocationStream) RecvMsg(m any) error {
	if err := context.Cause(s.ctx); err != nil {
		return err
	}
	return s.ServerStream.RecvMsg(m)
}

// Poll even idle streams. Returning from the interceptor closes the transport
// stream and unblocks pending RecvMsg calls; cancellation informs handlers to
// stop subprocesses and other work. Already completed side effects remain.
func (c *revocationChecker) streamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := c.checkRPC(stream.Context()); err != nil {
			return err
		}
		ctx, cancel := context.WithCancelCause(stream.Context())
		defer cancel(context.Canceled)
		wrapped := &revocationStream{ServerStream: stream, ctx: ctx}
		done := make(chan error, 1)
		go func() { done <- handler(srv, wrapped) }()
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for {
			select {
			case err := <-done:
				return err
			case <-ctx.Done():
				return status.FromContextError(ctx.Err()).Err()
			case <-ticker.C:
				if err := c.checkRPC(ctx); err != nil {
					cancel(err)
					return err
				}
			}
		}
	}
}
