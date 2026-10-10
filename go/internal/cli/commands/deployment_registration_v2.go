package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Numeric enrollments stay on the existing registration path. An unsupported
// v2 RPC is the only RPC failure permitting legacy fallback; malformed UUID
// enrollment never turns into an offline or numeric registration bypass.
func tryRegisterV2Apps(ctx context.Context, conn *grpcclient.AgentConnection, appIDs []string) (bool, error) {
	if conn == nil || conn.Conn == nil {
		return false, nil
	}
	var verifiedPeer peer.Peer
	response, err := agentpbv2.NewWendyProvisioningServiceClient(conn.Conn).IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{}, grpc.Peer(&verifiedPeer))
	if status.Code(err) == codes.Unimplemented {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	device := response.GetProvisioned()
	if device == nil {
		if response.GetNotProvisioned() != nil {
			return true, nil
		}
		return true, fmt.Errorf("device returned no v2 enrollment status")
	}
	if device.GetPrincipalUri() == "" && device.GetOrganizationId() > 0 && device.GetAssetId() > 0 {
		return false, nil
	}
	identity, err := deploymentV2Identity(device)
	if err != nil {
		return true, err
	}
	if !conn.IsMTLS && !conn.IsSessionProxy {
		return true, fmt.Errorf("Cloud v2 app registration requires an authenticated device connection")
	}
	if conn.IsSessionProxy {
		if conn.CertInfo == nil || conn.CertInfo.TenantUUID() != identity.TenantUUID {
			return true, fmt.Errorf("v2 provisioning disagrees with trusted proxy tenant")
		}
	} else {
		info, ok := verifiedPeer.AuthInfo.(credentials.TLSInfo)
		if !ok || !info.State.HandshakeComplete || len(info.State.PeerCertificates) == 0 {
			return true, fmt.Errorf("verified registration peer unavailable")
		}
		principal, ok := certs.TenantPrincipalFromCert(info.State.PeerCertificates[0])
		if !ok || principal != identity.Principal {
			return true, fmt.Errorf("v2 provisioning disagrees with authenticated TLS peer")
		}
	}
	if peer, verified := conn.ObservedServerIdentity(); verified && peer.Principal != identity.Principal {
		return true, fmt.Errorf("v2 provisioning disagrees with authenticated device identity")
	}
	cfg, err := config.Load()
	if err != nil {
		return true, err
	}
	auth, err := deploymentV2Auth(cfg, device.GetCloudHost(), identity.TenantUUID)
	if err != nil {
		return true, err
	}
	cloudConn, err := dialCloudGRPC(auth)
	if err != nil {
		return true, err
	}
	defer cloudConn.Close()
	rpcctx, err := cloudContext(ctx, auth)
	if err != nil {
		return true, err
	}
	client := cloudpbv2.NewAppServiceClient(cloudConn)
	upsert := func(ctx context.Context, req *cloudpbv2.UpsertAppRequest) (*cloudpbv2.App, error) {
		return upsertV2DeploymentApp(ctx, cloudConn, auth, req)
	}
	return true, registerV2AppCatalog(rpcctx, client, upsert, identity.TenantUUID, appIDs)
}

func upsertV2DeploymentApp(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, req *cloudpbv2.UpsertAppRequest) (*cloudpbv2.App, error) {
	reply := &cloudpbv2.App{}
	err := cloudrequest.Invoke(ctx, conn, auth, cloudpbv2.AppService_UpsertApp_FullMethodName, req, reply)
	return reply, err
}

func deploymentV2Identity(device *agentpbv2.ProvisionedResponse) (certs.WendyIdentity, error) {
	identity, err := certs.ParsePrincipal(device.GetPrincipalUri())
	if err != nil || identity.EntityType != certs.EntityAsset || device.GetCloudHost() == "" {
		return certs.WendyIdentity{}, fmt.Errorf("invalid Cloud v2 device enrollment")
	}
	for _, id := range []string{identity.TenantUUID, identity.EntityID} {
		parsed, err := uuid.Parse(id)
		if err != nil || parsed.String() != id {
			return certs.WendyIdentity{}, fmt.Errorf("Cloud v2 enrollment requires canonical UUID identities")
		}
	}
	return identity, nil
}

// Never let an Agent choose a new credential destination or match a UUID
// enrollment against the zero numeric organization shared by unrelated tenants.
func deploymentV2Auth(cfg *config.Config, host, tenant string) (*config.AuthConfig, error) {
	for _, entry := range cfg.Auth {
		if entry.CloudGRPC != host {
			continue
		}
		for _, certificate := range entry.Certificates {
			principal, err := certs.ParsePrincipal(certificate.CertificatePrincipal())
			if err != nil || principal.EntityType != certs.EntityUser || principal.TenantUUID != tenant {
				continue
			}
			selected := entry
			selected.Certificates = []config.CertificateInfo{certificate}
			return &selected, nil
		}
	}
	return nil, fmt.Errorf("no trusted Cloud operator session matches tenant %s at %s", tenant, host)
}

type v2DeploymentCatalog interface {
	GetApp(context.Context, *cloudpbv2.GetAppRequest, ...grpc.CallOption) (*cloudpbv2.App, error)
}

func registerV2AppCatalog(ctx context.Context, client v2DeploymentCatalog, upsert func(context.Context, *cloudpbv2.UpsertAppRequest) (*cloudpbv2.App, error), tenant string, appIDs []string) error {
	seen := map[string]bool{}
	for _, id := range appIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		app, err := client.GetApp(ctx, &cloudpbv2.GetAppRequest{Id: id, OrganizationId: tenant})
		if status.Code(err) == codes.NotFound {
			name := strings.TrimPrefix(id, "campaign:")
			// No notification grant or operator-edited metadata is overwritten.
			app, err = upsert(ctx, &cloudpbv2.UpsertAppRequest{Id: id, OrganizationId: tenant, Name: &name})
		}
		if err != nil {
			return fmt.Errorf("registering %s: %w", id, err)
		}
		if app.GetId() != id || app.GetOrganizationId() != tenant {
			return fmt.Errorf("Cloud returned a different app identity for %s", id)
		}
		cliLogln("Registered %s in Cloud Apps (tenant %s).", id, tenant)
		if !app.GetCanSendNotifications() {
			cliLogln("Notifications for %s are disabled; an owner or admin can enable its grant in the Cloud app settings.", id)
		}
	}
	return nil
}
