package interceptor

import (
	"context"
	"crypto/x509"
	"sync/atomic"

	"github.com/wendylabsinc/wendy/go/internal/agent/delegation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func delegatedScope(ctx context.Context) (*delegation.Scope, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing TLS peer")
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.PeerCertificates) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing TLS certificate")
	}
	s, err := delegation.Parse(ti.State.PeerCertificates[0])
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "invalid delegated certificate")
	}
	return s, nil
}

func authorizeDelegation(s *delegation.Scope, device, method string, req any) error {
	app := ""
	if getter, ok := req.(interface{ GetAppName() string }); ok {
		app = getter.GetAppName()
	}
	if err := s.Authorize(device, method, app); err != nil {
		return status.Error(codes.PermissionDenied, "delegated operation or resource is not permitted")
	}
	return nil
}

func UnaryDelegationInterceptor(device string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		s, err := delegatedScope(ctx)
		if err != nil {
			return nil, err
		}
		if s != nil {
			if err := authorizeDelegation(s, device, info.FullMethod, req); err != nil {
				return nil, err
			}
		}
		return handler(ctx, req)
	}
}

type delegatedStream struct {
	grpc.ServerStream
	scope          *delegation.Scope
	device, method string
	checked        atomic.Bool
}

func (s *delegatedStream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	if err := authorizeDelegation(s.scope, s.device, s.method, m); err != nil {
		return err
	}
	s.checked.Store(true)
	return nil
}
func (s *delegatedStream) SendMsg(m any) error {
	if !s.checked.Load() {
		return status.Error(codes.PermissionDenied, "delegated request has not been authorized")
	}
	return s.ServerStream.SendMsg(m)
}
func StreamDelegationInterceptor(device string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		s, err := delegatedScope(ss.Context())
		if err != nil {
			return err
		}
		if s == nil {
			return handler(srv, ss)
		}
		// Before invoking a handler, check its method/device authority. Request-level
		// app checks run before each decoded message reaches the handler.
		app := ""
		if len(s.Apps) > 0 {
			app = s.Apps[0]
		}
		if err := s.Authorize(device, info.FullMethod, app); err != nil {
			return status.Error(codes.PermissionDenied, "delegated operation or device is not permitted")
		}
		return handler(srv, &delegatedStream{ServerStream: ss, scope: s, device: device, method: info.FullMethod})
	}
}

// DelegatedCertificateForVerification acknowledges only a valid FleetScope on
// the device server. Generic verifiers continue rejecting this critical extension.
func DelegatedCertificateForVerification(leaf *x509.Certificate) (*x509.Certificate, error) {
	scope, err := delegation.Parse(leaf)
	if err != nil {
		return nil, err
	}
	if scope == nil {
		return leaf, nil
	}
	copyLeaf := *leaf
	copyLeaf.UnhandledCriticalExtensions = nil
	for _, oid := range leaf.UnhandledCriticalExtensions {
		if !oid.Equal(delegation.ScopeOID) {
			copyLeaf.UnhandledCriticalExtensions = append(copyLeaf.UnhandledCriticalExtensions, oid)
		}
	}
	return &copyLeaf, nil
}
