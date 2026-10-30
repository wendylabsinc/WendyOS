package commands

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	legacy "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type notificationCloudStub struct {
	grpc.ClientConnInterface
	writes                         int
	enabled, wrongRead, wrongWrite bool
	disable                        bool
	fail                           error
}

func (s *notificationCloudStub) Invoke(_ context.Context, method string, args, reply any, _ ...grpc.CallOption) error {
	switch method {
	case cloudpb.OperatorSessionService_RegisterOperatorLeaf_FullMethodName:
		return status.Error(codes.Unimplemented, "test Cloud uses full certificate signatures")
	case cloudpb.AppService_GetApp_FullMethodName:
		req := args.(*cloudpb.GetAppRequest)
		if req.Id != "sh.wendy.campaign.people" || req.OrganizationId != deploymentTenant {
			return errors.New("wrong read scope")
		}
		org := req.OrganizationId
		if s.wrongRead {
			org = "other"
		}
		proto.Merge(reply.(proto.Message), &cloudpb.App{Id: req.Id, OrganizationId: org, Name: "Keep name", Details: "Keep details", CanSendNotifications: s.enabled})
	case cloudpb.AppService_UpdateApp_FullMethodName:
		s.writes++
		if s.fail != nil {
			return s.fail
		}
		signed := args.(*cloudpb.SignedRequest)
		if signed.GetPayloadType() != "wendycloud.v2.UpdateAppRequest" || len(signed.GetSignature()) == 0 {
			return errors.New("missing signed grant")
		}
		var req cloudpb.UpdateAppRequest
		if err := proto.Unmarshal(signed.Payload, &req); err != nil {
			return err
		}
		if req.Id != "sh.wendy.campaign.people" || req.OrganizationId != deploymentTenant || req.Name != nil || req.Details != nil || req.CanSendNotifications == nil || *req.CanSendNotifications == s.disable {
			return errors.New("changed unreviewed fields")
		}
		org := req.OrganizationId
		if s.wrongWrite {
			org = "other"
		}
		proto.Merge(reply.(proto.Message), &cloudpb.App{Id: req.Id, OrganizationId: org, CanSendNotifications: !s.disable})
	case legacy.AppService_GetApp_FullMethodName:
		req := args.(*legacy.GetAppRequest)
		if req.Id != "sh.wendy.campaign.people" || req.OrganizationId != 7 {
			return errors.New("wrong legacy scope")
		}
		org := req.OrganizationId
		if s.wrongRead {
			org = 8
		}
		proto.Merge(reply.(proto.Message), &legacy.App{Id: req.Id, OrganizationId: org, CanSendNotifications: s.enabled})
	case legacy.AppService_UpdateApp_FullMethodName:
		s.writes++
		if s.fail != nil {
			return s.fail
		}
		req := args.(*legacy.UpdateAppRequest)
		if req.Id != "sh.wendy.campaign.people" || req.OrganizationId != 7 || req.Name != nil || req.Details != nil || req.CanSendNotifications == nil || *req.CanSendNotifications == s.disable {
			return errors.New("changed unreviewed legacy fields")
		}
		org := req.OrganizationId
		if s.wrongWrite {
			org = 8
		}
		proto.Merge(reply.(proto.Message), &legacy.App{Id: req.Id, OrganizationId: org, CanSendNotifications: !s.disable})
	default:
		return errors.New("unexpected RPC: " + method)
	}
	return nil
}
func TestCampaignCloudNotificationGrant(t *testing.T) {
	key := testKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	principal := "spiffe://wendy.sh/tenant/" + deploymentTenant + "/operator/test"
	auth := &config.AuthConfig{Certificates: []config.CertificateInfo{{PrincipalURI: principal,
		PemCertificate: liteLeafPEM(t, key, principal, time.Now().Add(time.Hour), false),
		PemPrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	}}}
	for _, device := range []*agentpb.ProvisionedResponse{deploymentV2Device(), {OrganizationId: 7}} {
		for _, scenario := range []string{"allow", "already enabled", "wrong read", "wrong write", "denied", "cancelled", "turn off"} {
			t.Run(device.GetPrincipalUri()+"/"+scenario, func(t *testing.T) {
				stub := &notificationCloudStub{enabled: scenario == "already enabled" || scenario == "turn off", disable: scenario == "turn off", wrongRead: scenario == "wrong read", wrongWrite: scenario == "wrong write"}
				if scenario == "denied" {
					stub.fail = status.Error(codes.PermissionDenied, "owner or admin required")
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if scenario == "cancelled" {
					cancel()
				}
				err := setCampaignCloudGrant(ctx, stub, auth, device, "sh.wendy.campaign.people", scenario != "turn off")
				if scenario == "allow" || scenario == "already enabled" || scenario == "turn off" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil {
					t.Fatal("expected grant failure")
				}
				wantWrites := 1
				if scenario == "already enabled" || scenario == "wrong read" || scenario == "cancelled" {
					wantWrites = 0
				}
				if stub.writes != wantWrites {
					t.Fatalf("writes=%d want=%d", stub.writes, wantWrites)
				}
				if scenario == "denied" && !strings.Contains(err.Error(), "owner or admin") {
					t.Fatalf("lost permission failure: %v", err)
				}
			})
		}
	}
}

type notificationCampaignDevice struct {
	agentpb.DataServiceClient
	campaigns []*agentpb.DataCampaign
}

func (s notificationCampaignDevice) Campaigns(context.Context, *agentpb.DataCampaignsRequest, ...grpc.CallOption) (*agentpb.DataCampaignsResponse, error) {
	return &agentpb.DataCampaignsResponse{Campaigns: s.campaigns}, nil
}
func TestCampaignNotificationGrantRequiresExistingNotifyingCampaign(t *testing.T) {
	for _, campaigns := range [][]*agentpb.DataCampaign{
		nil,
		{{Name: "other", PlanJson: []byte(`{"notify":{"on":"event"}}`)}},
		{{Name: "people", PlanJson: []byte(`{}`)}},
		{{Name: "people", PlanJson: []byte(`invalid`)}},
	} {
		// No provisioning client: these cases must fail before any Cloud access.
		conn := &grpcclient.AgentConnection{DataService: notificationCampaignDevice{campaigns: campaigns}}
		err := DesktopEnableCampaignNotifications(context.Background(), conn, "people")
		if err == nil || strings.Contains(err.Error(), "provisioning") {
			t.Fatalf("campaign validation was skipped: %v", err)
		}
	}
}
