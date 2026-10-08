package commands

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type cloudAppRegistration func(context.Context, *config.AuthConfig, *agentpbv2.ProvisionedResponse, []string) error

func registerCloudApps(ctx context.Context, conn *grpcclient.AgentConnection, appIDs []string, skip bool) error {
	if skip {
		cliLogln("Cloud app registration skipped (--skip-cloud-registration).")
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	response, err := deviceProvisioning(ctx, conn)
	if err != nil {
		err = fmt.Errorf("checking device enrollment: %w", err)
	} else {
		err = registerDeviceApps(ctx, response, appIDs, config.Load, registerAppsWithCloud)
	}
	if err != nil {
		return fmt.Errorf("registering deployment with Cloud: %w; use --skip-cloud-registration for an offline deployment", err)
	}
	return nil
}

func registerDeviceApps(ctx context.Context, response *agentpbv2.IsProvisionedResponse, appIDs []string,
	loadConfig func() (*config.Config, error), register cloudAppRegistration,
) error {
	device := response.GetProvisioned()
	if device == nil {
		if response.GetNotProvisioned() == nil {
			return fmt.Errorf("device returned no enrollment status")
		}
		return nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	auth, err := deploymentAuth(cfg, device)
	if err != nil {
		return err
	}
	// Compose services can share one app identity. Register it once per device.
	unique := make([]string, 0, len(appIDs))
	seen := make(map[string]bool, len(appIDs))
	for _, id := range appIDs {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	return register(ctx, auth, device, unique)
}

// Only dial an endpoint already trusted by a stored operator session. A device's
// enrollment response must never choose where we send another session's token.
//
// A PKI-enrolled device names its tenant in principal_uri and leaves the numeric
// IDs at 0; it needs an operator session (a user principal, never a device one)
// in that tenant. A legacy device has no principal and keeps the numeric match.
func deploymentAuth(cfg *config.Config, device *agentpbv2.ProvisionedResponse) (*config.AuthConfig, error) {
	host := device.GetCloudHost()
	var tenant string
	switch {
	case host == "":
		return nil, fmt.Errorf("device returned incomplete Cloud enrollment")
	case device.GetPrincipalUri() != "":
		identity, err := certs.ParsePrincipal(device.GetPrincipalUri())
		if err != nil {
			return nil, fmt.Errorf("device returned an invalid Cloud enrollment: %w", err)
		}
		tenant = identity.TenantUUID
	case device.GetOrganizationId() <= 0 || device.GetAssetId() <= 0:
		return nil, fmt.Errorf("device returned incomplete Cloud enrollment")
	}
	for _, entry := range cfg.Auth {
		if entry.CloudGRPC != host {
			continue
		}
		for _, cert := range entry.Certificates {
			var match bool
			if tenant != "" {
				operator, err := certs.ParsePrincipal(cert.PrincipalURI)
				match = err == nil && operator.TenantUUID == tenant && operator.EntityType == certs.EntityUser
			} else {
				match = cert.PrincipalURI == "" && cert.OrganizationID == int(device.GetOrganizationId()) && cert.UserID != "" && cert.AssetID == 0
			}
			if match {
				entry.Certificates = []config.CertificateInfo{cert}
				return &entry, nil
			}
		}
	}
	if tenant != "" {
		return nil, fmt.Errorf("no operator session for device tenant %s at %s; run 'wendy auth login' for that organization", tenant, host)
	}
	return nil, fmt.Errorf("no operator session for device organization %d at %s; run 'wendy auth login' for that organization", device.GetOrganizationId(), host)
}

// appCatalog returns the Cloud Apps entry for id, creating it only when it does
// not exist yet so operator-edited metadata and grants on an existing app stay.
type appCatalog func(ctx context.Context, id string) (*cloudpbv2.App, error)

func registerAppsWithCloud(ctx context.Context, auth *config.AuthConfig, device *agentpbv2.ProvisionedResponse, appIDs []string) error {
	conn, err := dialCloudGRPC(auth)
	if err != nil {
		return err
	}
	defer conn.Close()
	cloudCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return err
	}
	org, catalog := strconv.Itoa(int(device.GetOrganizationId())), legacyAppCatalog(conn, device.GetOrganizationId())
	if device.GetPrincipalUri() != "" {
		org = auth.OrganizationKey()
		catalog = tenantAppCatalog(conn, auth, org)
	}
	for _, id := range appIDs {
		app, err := catalog(cloudCtx, id)
		if err != nil {
			return fmt.Errorf("registering %s: %w", id, err)
		}
		if app.GetId() != id || app.GetOrganizationId() != org {
			return fmt.Errorf("Cloud returned a different app identity for %s", id)
		}
		cliLogln("Registered %s in Cloud Apps (organization %s).", id, org)
		if !app.GetCanSendNotifications() {
			cliLogln("Notifications for %s are disabled; an owner or admin can enable its grant in the Cloud app settings.", id)
		}
	}
	return nil
}

func tenantAppCatalog(conn grpc.ClientConnInterface, auth *config.AuthConfig, tenant string) appCatalog {
	client := cloudpbv2.NewAppServiceClient(conn)
	return func(ctx context.Context, id string) (*cloudpbv2.App, error) {
		app, err := client.GetApp(ctx, &cloudpbv2.GetAppRequest{Id: id, OrganizationId: tenant})
		if status.Code(err) == codes.NotFound {
			name := strings.TrimPrefix(id, "campaign:")
			app = &cloudpbv2.App{}
			err = cloudrequest.Invoke(ctx, conn, auth, cloudpbv2.AppService_UpsertApp_FullMethodName,
				&cloudpbv2.UpsertAppRequest{Id: id, OrganizationId: tenant, Name: &name}, app)
		}
		return app, err
	}
}

func legacyAppCatalog(conn grpc.ClientConnInterface, organization int32) appCatalog {
	client := cloudpb.NewAppServiceClient(conn)
	return func(ctx context.Context, id string) (*cloudpbv2.App, error) {
		app, err := client.GetApp(ctx, &cloudpb.GetAppRequest{Id: id, OrganizationId: organization})
		if status.Code(err) == codes.NotFound {
			name := strings.TrimPrefix(id, "campaign:")
			app, err = client.UpsertApp(ctx, &cloudpb.UpsertAppRequest{Id: id, OrganizationId: organization, Name: &name})
		}
		if err != nil {
			return nil, err
		}
		return &cloudpbv2.App{
			Id: app.GetId(), OrganizationId: strconv.Itoa(int(app.GetOrganizationId())),
			CanSendNotifications: app.GetCanSendNotifications(),
		}, nil
	}
}
