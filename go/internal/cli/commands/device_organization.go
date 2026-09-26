package commands

import (
	"context"
	"strconv"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type deviceOrganizationInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

func (o *deviceOrganizationInfo) label() string {
	if o.ID == "" {
		return "Not enrolled"
	}
	if o.Name != "" {
		return o.Name + " (" + o.ID + ")"
	}
	return o.ID
}

// A nil result means enrollment is unknown; an empty ID means the agent
// explicitly reported that it is not enrolled. Neither lookup may fail info.
func deviceOrganization(ctx context.Context, conn *grpcclient.AgentConnection) *deviceOrganizationInfo {
	if conn == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, orgNameResolveTimeout)
	defer cancel()

	resp, err := agentpbv2.NewWendyProvisioningServiceClient(conn.Conn).IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{})
	if status.Code(err) == codes.Unimplemented {
		legacy, legacyErr := conn.ProvisioningService.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
		if legacyErr != nil {
			return nil
		}
		if legacy.GetNotProvisioned() != nil {
			return &deviceOrganizationInfo{}
		}
		if prov := legacy.GetProvisioned(); prov != nil {
			resp = &agentpbv2.IsProvisionedResponse{ResponseType: &agentpbv2.IsProvisionedResponse_Provisioned{
				Provisioned: &agentpbv2.ProvisionedResponse{CloudHost: prov.GetCloudHost(), OrganizationId: prov.GetOrganizationId()},
			}}
		}
	} else if err != nil {
		return nil
	}
	if resp.GetNotProvisioned() != nil {
		return &deviceOrganizationInfo{}
	}
	prov := resp.GetProvisioned()
	if prov == nil {
		return nil
	}

	org := &deviceOrganizationInfo{}
	if principal := prov.GetPrincipalUri(); principal != "" {
		identity, err := certs.ParsePrincipal(principal)
		if err != nil {
			return nil
		}
		org.ID = identity.TenantUUID
	} else if prov.GetOrganizationId() > 0 {
		org.ID = strconv.FormatInt(int64(prov.GetOrganizationId()), 10)
	}
	if org.ID == "" {
		return nil
	}
	org.Name = deviceOrganizationName(ctx, prov.GetCloudHost(), org.ID)
	return org
}

func deviceOrganizationName(ctx context.Context, endpoint, organization string) string {
	cached := cachedOrganizationName(endpoint, organization)
	cfg, err := config.Load()
	if err != nil {
		return cached
	}
	for _, auth := range cfg.Auth {
		if auth.CloudGRPC != endpoint {
			continue
		}
		for _, cert := range auth.Certificates {
			lookup := auth
			lookup.Certificates = []config.CertificateInfo{cert}
			if lookup.OrganizationKey() == organization {
				return cloudOrganizationName(ctx, &lookup)
			}
		}
	}
	return cached
}
