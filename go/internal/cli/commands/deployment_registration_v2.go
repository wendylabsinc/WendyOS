package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	legacyagentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type deploymentProvisioningService struct{ conn *grpcclient.AgentConnection }

// V2 exposes the certificate principal even before Cloud's roster has synced.
// Only agents without the V2 RPC may fall back to numeric enrollment metadata.
func (s deploymentProvisioningService) IsProvisioned(ctx context.Context, req *agentpb.IsProvisionedRequest, opts ...grpc.CallOption) (*agentpb.IsProvisionedResponse, error) {
	if s.conn == nil {
		return nil, fmt.Errorf("device provisioning service is unavailable")
	}
	var remote peer.Peer
	opts = append(opts, grpc.Peer(&remote))
	if s.conn.Conn != nil {
		response, err := agentpb.NewWendyProvisioningServiceClient(s.conn.Conn).IsProvisioned(ctx, req, opts...)
		if err == nil {
			if err := validateDeploymentEnrollment(s.conn, &remote, response.GetProvisioned()); err != nil {
				return nil, err
			}
		}
		if status.Code(err) != codes.Unimplemented {
			return response, err
		}
	}
	if s.conn.ProvisioningService == nil {
		return nil, fmt.Errorf("device provisioning service is unavailable")
	}
	response, err := s.conn.ProvisioningService.IsProvisioned(ctx, &legacyagentpb.IsProvisionedRequest{}, opts...)
	if err != nil {
		return nil, err
	}
	if device := response.GetProvisioned(); device != nil {
		provisioned := &agentpb.ProvisionedResponse{CloudHost: device.GetCloudHost(), OrganizationId: device.GetOrganizationId(), AssetId: device.GetAssetId()}
		if err := validateDeploymentEnrollment(s.conn, &remote, provisioned); err != nil {
			return nil, err
		}
		return &agentpb.IsProvisionedResponse{ResponseType: &agentpb.IsProvisionedResponse_Provisioned{
			Provisioned: provisioned,
		}}, nil
	}
	if response.GetNotProvisioned() != nil {
		return &agentpb.IsProvisionedResponse{ResponseType: &agentpb.IsProvisionedResponse_NotProvisioned{
			NotProvisioned: &agentpb.NotProvisionedResponse{},
		}}, nil
	}
	return nil, fmt.Errorf("device returned no enrollment status")
}

// Enrollment is device-controlled data, so it cannot select another stored
// tenant's operator credentials. Bind it before configuration or Cloud access.
func validateDeploymentEnrollment(conn *grpcclient.AgentConnection, remote *peer.Peer, device *agentpb.ProvisionedResponse) error {
	if device == nil {
		return nil
	}
	if conn == nil {
		return fmt.Errorf("device connection unavailable for Cloud registration")
	}
	operatorTenant := ""
	if conn.CertInfo != nil {
		operatorTenant = conn.CertInfo.TenantUUID()
	}
	expectedPrincipal := ""
	if conn.IsSessionProxy {
		// The owner-only local broker retains the original verified identity
		// and the exact operator certificate used by its pinned upstream.
		if conn.IsMTLS {
			if identity, ok := conn.ObservedServerIdentity(); ok {
				expectedPrincipal = identity.Principal
			}
		}
	} else if remote != nil {
		if info, ok := remote.AuthInfo.(credentials.TLSInfo); ok && info.State.HandshakeComplete && len(info.State.PeerCertificates) != 0 {
			expectedPrincipal, _ = certs.TenantPrincipalFromCert(info.State.PeerCertificates[0])
		}
	}
	if device.GetPrincipalUri() == "" {
		if expectedPrincipal != "" || operatorTenant != "" {
			return fmt.Errorf("Agent enrollment omits its authenticated Cloud principal")
		}
		return nil // Preserve numeric enrollment on legacy connections.
	}
	identity, err := certs.ParsePrincipal(device.GetPrincipalUri())
	if err != nil || identity.EntityType != certs.EntityAsset || identity.TenantUUID == "" {
		return fmt.Errorf("device returned invalid Cloud enrollment principal")
	}
	if operatorTenant != "" && identity.TenantUUID != operatorTenant {
		return fmt.Errorf("Agent enrollment disagrees with the connection operator tenant")
	}
	if expectedPrincipal != "" {
		if device.GetPrincipalUri() != expectedPrincipal {
			return fmt.Errorf("Agent enrollment disagrees with its verified device identity")
		}
		return nil
	}
	// Older broker state may retain only a numeric device identity. The
	// operator's authenticated tenant still prevents cross-tenant registration,
	// though that state cannot prove the response's full device principal.
	if conn.IsSessionProxy && conn.IsMTLS && operatorTenant != "" {
		return nil
	}
	return fmt.Errorf("verified device identity unavailable for Cloud registration")
}

func registerAppsWithCloudV2(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, device *agentpb.ProvisionedResponse, appIDs []string) error {
	identity, err := certs.ParsePrincipal(device.GetPrincipalUri())
	if err != nil || identity.EntityType != certs.EntityAsset || identity.TenantUUID == "" {
		return fmt.Errorf("device returned invalid Cloud enrollment principal")
	}
	tenant := identity.TenantUUID
	client := cloudpb.NewAppServiceClient(conn)
	for _, id := range appIDs {
		app, err := client.GetApp(ctx, &cloudpb.GetAppRequest{Id: id, OrganizationId: tenant})
		if status.Code(err) == codes.NotFound {
			name := strings.TrimPrefix(id, "campaign:")
			app = new(cloudpb.App)
			err = cloudrequest.Invoke(ctx, conn, auth, cloudpb.AppService_UpsertApp_FullMethodName,
				&cloudpb.UpsertAppRequest{Id: id, OrganizationId: tenant, Name: &name}, app)
		}
		if err != nil {
			return fmt.Errorf("registering %s: %w", id, err)
		}
		if app.GetId() != id || app.GetOrganizationId() != tenant {
			return fmt.Errorf("Cloud returned a different app identity for %s", id)
		}
		cliLogln("Registered %s in Cloud Apps (organization %s).", id, tenant)
		if !app.GetCanSendNotifications() {
			cliLogln("Notifications for %s are disabled; an owner or admin can enable its grant in the Cloud app settings.", id)
		}
	}
	return nil
}
