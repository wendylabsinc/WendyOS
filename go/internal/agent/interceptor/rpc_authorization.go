package interceptor

import (
	"context"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/rpcauth"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// deviceRPCAllowed is deliberately an exact-method allowlist, not a service or
// read-only category. A newly registered RPC remains unavailable to device
// credentials until its device use case is explicitly reviewed here.
func deviceRPCAllowed(method string) bool {
	switch method {
	// A build host stages image content on a target. These methods do not
	// create, run, remove or execute a container. Per-job delegation remains a
	// separate authorization concern; device identity is not a build grant.
	case agentpb.WendyContainerService_QueryChunks_FullMethodName,
		agentpb.WendyContainerService_QueryLayers_FullMethodName,
		agentpb.WendyContainerService_WriteChunks_FullMethodName,
		agentpb.WendyContainerService_PrepareImage_FullMethodName:
		return true
	// Legacy peer agents carry application TCP streams through MeshDial.
	// Its handler independently restricts the tenant and running-app ingress.
	case agentpbv2.WendyMeshService_MeshDial_FullMethodName:
		return true
	// Agent-hosted SensorLink uses this dedicated catalogue/stream API, not
	// the general camera, audio or sensor-pairing administration services.
	// Swift serves it today; keep the grant for a future Go implementation.
	case agentpbv2.WendySensorService_GetSensorManifest_FullMethodName,
		agentpbv2.WendySensorService_StreamSensors_FullMethodName:
		return true
	default:
		return false
	}
}

// CheckRPCIdentity authorizes an already authenticated mTLS caller. It must run
// after CheckMTLS: role claims come only from the verified leaf, never request
// metadata. Operator/user callers retain the existing handler-specific policy.
// Device callers need both an exact allowlisted method and a provably matching
// tenant, even when the authentication gate is configured for org migration.
// Identity-less, malformed and non-user/non-device credentials fail closed.
func CheckRPCIdentity(ctx context.Context, method string, expected certs.Scope) error {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "mTLS caller identity required")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || !tlsInfo.State.HandshakeComplete || len(tlsInfo.State.PeerCertificates) == 0 {
		return status.Error(codes.Unauthenticated, "verified mTLS caller identity required")
	}
	identity, found, err := certs.IdentityFromCert(tlsInfo.State.PeerCertificates[0])
	if err != nil || !found {
		return rpcauth.Denied("client certificate has no recognized RPC caller identity")
	}
	switch identity.EntityType {
	case certs.EntityUser:
		return nil
	case certs.EntityAsset:
		if !expected.Known() || !identity.Scope().Matches(expected) {
			return rpcauth.Denied("device RPC requires a matching tenant identity")
		}
		if deviceRPCAllowed(method) {
			return nil
		}
		return rpcauth.Denied("RPC is not permitted for device credentials; sign in with operator credentials")
	default:
		return rpcauth.Denied("client certificate role is not permitted for agent RPCs")
	}
}

func logRPCIdentityRefusal(ctx context.Context, logger *zap.Logger, method string, err error) {
	if logger == nil {
		return
	}
	fields := []zap.Field{
		zap.String("method", method),
		zap.String("remote", peerAddr(ctx)),
		zap.Error(err),
	}
	if p, ok := peer.FromContext(ctx); ok {
		if info, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(info.State.PeerCertificates) > 0 {
			if serial := info.State.PeerCertificates[0].SerialNumber; serial != nil {
				fields = append(fields, zap.String("serial", serial.String()))
			}
		}
	}
	logger.Warn("rejected gRPC caller by RPC identity policy", fields...)
}

func UnaryRPCIdentityInterceptor(logger *zap.Logger, expected certs.Scope) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if err := CheckRPCIdentity(ctx, info.FullMethod, expected); err != nil {
			logRPCIdentityRefusal(ctx, logger, info.FullMethod, err)
			return nil, err
		}
		return handler(ctx, req)
	}
}

func StreamRPCIdentityInterceptor(logger *zap.Logger, expected certs.Scope) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := CheckRPCIdentity(ss.Context(), info.FullMethod, expected); err != nil {
			logRPCIdentityRefusal(ss.Context(), logger, info.FullMethod, err)
			return err
		}
		return handler(srv, ss)
	}
}
