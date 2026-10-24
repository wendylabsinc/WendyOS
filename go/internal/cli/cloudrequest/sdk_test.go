package cloudrequest

import (
	"context"
	"encoding/base64"
	"github.com/google/uuid"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
	"time"
)

const brokerAudience = "https://cloud.wendy.sh/broker"
const signatureTTL = 30 * time.Second

func (s *Signer) signRequest(method string, request proto.Message, byKID bool, correlation string) (*cloudpbv2.SignedRequest, error) {
	shared, err := s.sdk(context.Background())
	if err != nil {
		return nil, err
	}
	signed, err := shared.SignRequest(context.Background(), method, request, byKID, correlation)
	if err != nil {
		return nil, err
	}
	wire, err := proto.Marshal(signed)
	if err != nil {
		return nil, err
	}
	result := new(cloudpbv2.SignedRequest)
	if err := proto.Unmarshal(wire, result); err != nil {
		return nil, err
	}
	return result, nil
}

func testX5C(s *Signer) []string {
	encoded := make([]string, len(s.chain))
	for i, der := range s.chain {
		encoded[i] = base64.StdEncoding.EncodeToString(der)
	}
	return encoded
}

var signedResources = map[string]func(tenant string, req proto.Message) (string, bool){
	cloudpbv2.OperatorSessionService_RegisterOperatorLeaf_FullMethodName: func(tenant string, req proto.Message) (string, bool) {
		in, ok := req.(*cloudpbv2.RegisterOperatorLeafRequest)
		return "org/" + in.GetOrganizationId() + "/operator-leaf", ok
	},
	cloudpbv2.DeviceEnrollmentService_EnrollDevice_FullMethodName: func(tenant string, req proto.Message) (string, bool) {
		in, ok := req.(*cloudpbv2.EnrollDeviceRequest)
		return "org/" + tenant + "/device/" + in.GetDeviceId(), ok
	},
	cloudpbv2.AppService_UpsertApp_FullMethodName: func(tenant string, req proto.Message) (string, bool) {
		in, ok := req.(*cloudpbv2.UpsertAppRequest)
		if !ok || in.GetOrganizationId() != tenant || in.GetId() == "" {
			return "", false
		}
		return "org/" + tenant + "/app/" + in.GetId(), true
	},
	// Cloud DeploymentServiceHandler createAppRelease / updateAppRelease.
	cloudpbv2.DeploymentService_CreateAppRelease_FullMethodName: func(tenant string, req proto.Message) (string, bool) {
		in, ok := req.(*cloudpbv2.CreateAppReleaseRequest)
		if !ok || in.GetOrganizationId() != tenant || in.GetAppId() == "" {
			return "", false
		}
		return "org/" + tenant + "/app/" + in.GetAppId(), true
	},
	cloudpbv2.DeploymentService_UpdateAppRelease_FullMethodName: func(_ string, req proto.Message) (string, bool) {
		in, ok := req.(*cloudpbv2.UpdateAppReleaseRequest)
		if !ok {
			return "", false
		}
		id, err := uuid.Parse(in.GetId())
		if err != nil || id.String() != in.GetId() {
			return "", false
		}
		return "app-release/" + in.GetId(), true
	},
	// Ruled 2026-10-13 (WDY-3660); cloud's RequestSigning.md row follows.
	cloudpbv2.RelayMcpService_OpenRelaySession_FullMethodName: func(tenant string, req proto.Message) (string, bool) {
		in, ok := req.(*cloudpbv2.OpenRelaySessionRequest)
		return "org/" + in.GetOrganizationId() + "/relay-session", ok
	},
	cloudpbv2.AssetService_DeleteAsset_FullMethodName: func(_ string, req proto.Message) (string, bool) {
		in, ok := req.(*cloudpbv2.DeleteAssetRequest)
		if !ok {
			return "", false
		}
		id, err := uuid.Parse(in.GetId())
		if err != nil || id.String() != in.GetId() {
			return "", false
		}
		return "asset/" + in.GetId(), true
	},
	cloudpbv2.PKIAdminService_RevokePrincipalCertificates_FullMethodName: func(tenant string, req proto.Message) (string, bool) {
		in, ok := req.(*cloudpbv2.RevokePrincipalCertificatesRequest)
		return "org/" + tenant + "/pki", ok && in.GetOrganizationId() == tenant
	},
	cloudpbv2.PKIAdminService_RevokeEnrollmentToken_FullMethodName: func(tenant string, req proto.Message) (string, bool) {
		in, ok := req.(*cloudpbv2.RevokeEnrollmentTokenRequest)
		return "org/" + tenant + "/pki", ok && in.GetOrganizationId() == tenant && in.GetTokenId() != ""
	},
	cloudpbv2.PKIAdminService_RevokeEABKey_FullMethodName: func(tenant string, req proto.Message) (string, bool) {
		in, ok := req.(*cloudpbv2.RevokeEABKeyRequest)
		return "org/" + tenant + "/pki", ok && in.GetOrganizationId() == tenant && in.GetKeyId() != ""
	},
}
