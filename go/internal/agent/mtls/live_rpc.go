package mtls

import (
	"context"
	"crypto/x509"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Recheck existing TLS connections at RPC/message boundaries. Handshake-only
// checks would let an HTTP/2 connection outlive a newly published revocation.
func livePeerCheck(verify func([][]byte, [][]*x509.Certificate) error) func(context.Context) error {
	return func(ctx context.Context) error {
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
		if err := verify(raw, nil); err != nil {
			return status.Errorf(codes.Unauthenticated, "peer certificate: %v", err)
		}
		return nil
	}
}

func liveUnaryInterceptor(check func(context.Context) error) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := check(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}
func liveStreamInterceptor(check func(context.Context) error) grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := check(stream.Context()); err != nil {
			return err
		}
		return handler(srv, &liveServerStream{ServerStream: stream, check: check})
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
