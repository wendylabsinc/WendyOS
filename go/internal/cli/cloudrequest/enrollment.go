package cloudrequest

import (
	"context"
	"fmt"

	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy-go/cloud"
	"github.com/wendylabsinc/wendy-go/enrollment"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	pkipb "github.com/wendylabsinc/wendy/go/proto/gen/pkicorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// EnrollmentSession projects public selected-session identity without key material.
func EnrollmentSession(auth *config.AuthConfig) enrollment.Session {
	if auth == nil {
		return enrollment.Session{}
	}
	session := enrollment.Session{PKIEndpoint: auth.PKIEndpoint}
	if len(auth.Certificates) > 0 {
		session.Principal = auth.Certificates[0].PrincipalURI
		if session.Principal == "" {
			session.Principal = auth.Certificates[0].CertificatePrincipal()
		}
	}
	return session
}

// EnrollmentClient borrows the caller's authenticated connection and key store.
// The transport preserves the CLI's typed call/interceptor and diagnostic boundary.
func EnrollmentClient(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig) *enrollment.Client {
	md, _ := metadata.FromOutgoingContext(ctx)
	return &enrollment.Client{Cloud: EnrollmentTransport(conn), Entropy: wendy.CryptoEntropy{}, Signer: enrollmentSigner{auth}, Metadata: coreMetadata(md)}
}

// enrollmentSigner opens credentials only when Core requests a signature. Keep
// key failures behind the original readiness, binding and inventory checks.
type enrollmentSigner struct{ auth *config.AuthConfig }

func (s enrollmentSigner) EnrollmentRequest(ctx context.Context, deviceID, deviceClass, chain string) ([]byte, error) {
	host, err := newSigner(s.auth)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, fmt.Errorf("device enrollment requires an operator certificate")
	}
	signer, err := host.sdk(ctx)
	if err != nil {
		return nil, err
	}
	return signer.EnrollmentRequest(ctx, deviceID, deviceClass, chain)
}
func (s enrollmentSigner) Invoke(ctx context.Context, transport wendy.Transport, method string, request proto.Message, options cloud.InvokeOptions) (wendy.RPCResponse, error) {
	host, err := newSigner(s.auth)
	if err != nil {
		return wendy.RPCResponse{}, err
	}
	if host == nil {
		return wendy.RPCResponse{}, fmt.Errorf("%s requires an operator certificate; run 'wendy auth login'", method)
	}
	signer, err := host.sdk(ctx)
	if err != nil {
		return wendy.RPCResponse{}, err
	}
	return signer.Invoke(ctx, transport, method, request, options)
}

// EnrollmentTransport converts private SDK protobufs to the CLI's generated types.
// Networking, TLS and authentication remain on the supplied grpc-go connection.
// The exact generated request/response types preserve existing interceptors.
func EnrollmentTransport(conn grpc.ClientConnInterface) wendy.Transport {
	return enrollmentTransport{conn}
}

type enrollmentTransport struct{ conn grpc.ClientConnInterface }

func (t enrollmentTransport) Unary(ctx context.Context, r wendy.RPCRequest) (wendy.RPCResponse, error) {
	if err := ctx.Err(); err != nil {
		return wendy.RPCResponse{}, err
	}
	request, reply, err := enrollmentMessages(r.Method)
	if err != nil {
		return wendy.RPCResponse{}, err
	}
	if err := proto.Unmarshal(r.Message, request); err != nil {
		return wendy.RPCResponse{}, err
	}
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	for name, values := range grpcMetadata(r.Metadata) {
		md[name] = values
	}
	ctx = metadata.NewOutgoingContext(ctx, md)
	var headers, trailers metadata.MD
	err = t.conn.Invoke(ctx, r.Method, request, reply, grpc.Header(&headers), grpc.Trailer(&trailers))
	response := wendy.RPCResponse{InitialMetadata: coreMetadata(headers), TrailingMetadata: coreMetadata(trailers)}
	if err != nil {
		return response, EnrollmentError(err, headers, trailers)
	}
	response.Message, err = proto.Marshal(reply)
	return response, err
}
func (enrollmentTransport) ServerStream(context.Context, wendy.RPCRequest) (wendy.ServerStream, error) {
	return nil, fmt.Errorf("enrollment requests are unary")
}

// EnrollmentError retains grpc-go status/details and the established CLI text
// while exposing Core's transport-neutral status and binary metadata to policy.
func EnrollmentError(err error, headers, trailers metadata.MD) error {
	if err == nil {
		return nil
	}
	st := status.Convert(err)
	return &displayRPCError{status: &wendy.RPCStatusError{Code: wendy.RPCStatusCode(st.Code()), Message: st.Message(), InitialMetadata: coreMetadata(headers), TrailingMetadata: coreMetadata(trailers), Cause: err}}
}

// EnrollmentMetadata converts gRPC metadata without dropping repeated values.
func EnrollmentMetadata(md metadata.MD) wendy.Metadata { return coreMetadata(md) }

func enrollmentMessages(method string) (proto.Message, proto.Message, error) {
	switch method {
	case agentpb.WendyProvisioningService_IsProvisioned_FullMethodName:
		return new(agentpb.IsProvisionedRequest), new(agentpb.IsProvisionedResponse), nil
	case agentpb.WendyDeviceInfoService_GetDeviceInfo_FullMethodName:
		return new(agentpb.GetDeviceInfoRequest), new(agentpb.GetDeviceInfoResponse), nil
	case agentpb.WendyProvisioningService_StartACMEProvisioning_FullMethodName:
		return new(agentpb.StartACMEProvisioningRequest), new(agentpb.StartACMEProvisioningResponse), nil
	case pb.AssetService_GetAsset_FullMethodName:
		return new(pb.GetAssetRequest), new(pb.Asset), nil
	case pb.PKIAdminService_ListEnrollmentTokens_FullMethodName:
		return new(pb.ListEnrollmentTokensRequest), new(pkipb.ListEnrollmentTokensResponse), nil
	case pb.PKIAdminService_ListEABKeys_FullMethodName:
		return new(pb.ListEABKeysRequest), new(pkipb.ListEABKeysResponse), nil
	case pb.OperatorSessionService_RegisterOperatorLeaf_FullMethodName:
		return new(pb.SignedRequest), new(pb.RegisterOperatorLeafResponse), nil
	case pb.DeviceEnrollmentService_EnrollDevice_FullMethodName:
		return new(pb.SignedRequest), new(pb.EnrollDeviceResponse), nil
	case pb.PKIAdminService_RevokeEnrollmentToken_FullMethodName:
		return new(pb.SignedRequest), new(pkipb.RevokeEnrollmentTokenResponse), nil
	case pb.PKIAdminService_RevokeEABKey_FullMethodName:
		return new(pb.SignedRequest), new(pkipb.RevokeEABKeyResponse), nil
	case pb.PKIAdminService_RevokePrincipalCertificates_FullMethodName:
		return new(pb.SignedRequest), new(pkipb.RevokeByPrincipalResponse), nil
	case pb.AssetService_DeleteAsset_FullMethodName:
		return new(pb.SignedRequest), new(pb.DeleteAssetResponse), nil
	default:
		return nil, nil, fmt.Errorf("unknown enrollment RPC %s", method)
	}
}
