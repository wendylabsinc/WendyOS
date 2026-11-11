package interceptor

import (
	"context"
	"crypto/x509"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/operatorpermission"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func permissionLeaf(ctx context.Context) *x509.Certificate {
	if p, ok := peer.FromContext(ctx); ok {
		if info, ok := p.AuthInfo.(credentials.TLSInfo); ok && info.State.HandshakeComplete && len(info.State.PeerCertificates) > 0 {
			return info.State.PeerCertificates[0]
		}
	}
	return nil
}

func checkOperatorRPC(ctx context.Context, p *operatorpermission.Policy, method string) (*x509.Certificate, error) {
	leaf := permissionLeaf(ctx)
	// The preceding identity interceptor separately verifies these peer-device
	// exceptions. Operator entitlements never expand the device allowlist.
	if leaf != nil {
		if id, found, err := certs.IdentityFromCert(leaf); err == nil && found && id.EntityType == certs.EntityAsset && deviceRPCAllowed(method) {
			return nil, nil
		}
	}
	if err := p.Check(leaf, operatorpermission.RPCPermission(method), method); err != nil {
		return leaf, err
	}
	for _, command := range metadata.ValueFromIncomingContext(ctx, appconfig.PostStartAgentHookMetadataKey) {
		if command != "" {
			if err := p.Check(leaf, "device:shell", "agent post-start hook"); err != nil {
				return leaf, err
			}
			break
		}
	}
	return leaf, nil
}

func UnaryOperatorPermissionInterceptor(p *operatorpermission.Policy) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if _, err := checkOperatorRPC(ctx, p, info.FullMethod); err != nil {
			return nil, err
		}
		if err := checkOperatorRequest(ctx, p, info.FullMethod, req); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

type permissionStream struct {
	grpc.ServerStream
	ctx    context.Context
	policy *operatorpermission.Policy
	method string
}

func (s *permissionStream) Context() context.Context { return s.ctx }
func (s *permissionStream) RecvMsg(msg any) error {
	if err := s.ServerStream.RecvMsg(msg); err != nil {
		return err
	}
	return checkOperatorRequest(s.ctx, s.policy, s.method, msg)
}
func checkOperatorRequest(ctx context.Context, p *operatorpermission.Policy, method string, req any) error {
	switch method {
	case "/wendy.agent.services.v1.WendyContainerService/CreateContainer", "/wendy.agent.services.v1.WendyContainerService/CreateContainerWithProgress", "/wendy.agent.services.v1.WendyContainerService/RunContainer":
	default:
		return nil
	}
	permission, err := operatorpermission.RequestPermission(req)
	if err != nil {
		return err
	}
	if permission == "" {
		return nil
	}
	return p.Check(permissionLeaf(ctx), permission, "privileged app configuration")
}

func StreamOperatorPermissionInterceptor(p *operatorpermission.Policy) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		leaf, err := checkOperatorRPC(ss.Context(), p, info.FullMethod)
		if err != nil {
			return err
		}
		if leaf == nil {
			return handler(srv, ss)
		}
		if !p.Enforce {
			return handler(srv, &permissionStream{ServerStream: ss, ctx: ss.Context(), policy: p, method: info.FullMethod})
		}
		now := time.Now()
		if p.Now != nil {
			now = p.Now()
		}
		ctx, cancel := context.WithTimeout(ss.Context(), leaf.NotAfter.Sub(now))
		defer cancel()
		// Returning on expiry also terminates the underlying gRPC stream, including
		// a handler blocked in RecvMsg. The child context cancels shell processes.
		done := make(chan error, 1)
		go func() {
			done <- handler(srv, &permissionStream{ServerStream: ss, ctx: ctx, policy: p, method: info.FullMethod})
		}()
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
}
