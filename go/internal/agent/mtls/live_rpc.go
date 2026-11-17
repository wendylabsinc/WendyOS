package mtls

import (
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/shared/revocation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Every authenticated RPC needs current evidence when a fleet grace is configured.
// Stream message boundaries only check positive revocation, so an outage does
// not interrupt work already admitted. With no configured grace, both levels
// retain the offline policy and refuse only known revocations.
func livePeerCheck(verify peerCheck, level revocation.Level) func(context.Context, string) error {
	return func(ctx context.Context, method string) error {
		p, ok := peer.FromContext(ctx)
		if !ok {
			return status.Error(codes.Unauthenticated, "missing TLS peer")
		}
		info, ok := p.AuthInfo.(credentials.TLSInfo)
		if !ok {
			return status.Error(codes.Unauthenticated, "missing TLS peer")
		}
		raw := make([][]byte, len(info.State.PeerCertificates))
		for i, c := range info.State.PeerCertificates {
			raw[i] = c.Raw
		}
		err := verify(raw, level)
		if errors.Is(err, revocation.ErrUnavailable) {
			return status.Errorf(codes.FailedPrecondition, "%s needs current certificate revocation evidence, which this device does not have: bring the device or this computer online and retry (%v)", method, err)
		}
		if err != nil {
			return status.Errorf(codes.Unauthenticated, "peer certificate: %v", err)
		}
		return nil
	}
}

func liveUnaryInterceptor(check func(context.Context, string) error) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := check(ctx, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}
func liveStreamInterceptor(check, messageCheck func(context.Context, string) error) grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := check(stream.Context(), info.FullMethod); err != nil {
			return err
		}
		// Message boundaries only look for a revocation published mid-stream.
		return handler(srv, &liveServerStream{ServerStream: stream, check: func(ctx context.Context) error { return messageCheck(ctx, "") }})
	}
}

type liveServerStream struct {
	grpc.ServerStream
	check func(context.Context) error
}

func (s *liveServerStream) SendMsg(m any) error {
	if err := s.check(s.Context()); err != nil {
		return err
	}
	return s.ServerStream.SendMsg(m)
}
func (s *liveServerStream) RecvMsg(m any) error {
	if err := s.check(s.Context()); err != nil {
		return err
	}
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	return s.check(s.Context())
}
