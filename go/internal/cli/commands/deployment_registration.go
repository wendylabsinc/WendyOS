package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type deploymentProvisioningClient interface {
	IsProvisioned(context.Context, *agentpb.IsProvisionedRequest, ...grpc.CallOption) (*agentpb.IsProvisionedResponse, error)
}

type cloudAppRegistration func(context.Context, *config.AuthConfig, *agentpb.ProvisionedResponse, []string) error

func registerCloudApps(ctx context.Context, conn *grpcclient.AgentConnection, appIDs []string, skip bool) error {
	if skip {
		cliLogln("Cloud app registration skipped (--skip-cloud-registration).")
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := registerDeviceApps(ctx, conn.ProvisioningService, appIDs, config.Load, registerAppsWithCloud); err != nil {
		return fmt.Errorf("registering deployment with Cloud: %w; use --skip-cloud-registration for an offline deployment", err)
	}
	return nil
}

func registerDeviceApps(ctx context.Context, provisioning deploymentProvisioningClient, appIDs []string,
	loadConfig func() (*config.Config, error), register cloudAppRegistration,
) error {
	if provisioning == nil {
		return fmt.Errorf("device provisioning service is unavailable")
	}
	response, err := provisioning.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
	if err != nil {
		return fmt.Errorf("checking device enrollment: %w", err)
	}
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
func deploymentAuth(cfg *config.Config, device *agentpb.ProvisionedResponse) (*config.AuthConfig, error) {
	if device.GetCloudHost() == "" || device.GetOrganizationId() <= 0 || device.GetAssetId() <= 0 {
		return nil, fmt.Errorf("device returned incomplete Cloud enrollment")
	}
	for _, entry := range cfg.Auth {
		if entry.CloudGRPC != device.GetCloudHost() {
			continue
		}
		for _, cert := range entry.Certificates {
			if cert.OrganizationID == int(device.GetOrganizationId()) && cert.UserID != "" && cert.AssetID == 0 {
				entry.Certificates = []config.CertificateInfo{cert}
				return &entry, nil
			}
		}
	}
	return nil, fmt.Errorf("no operator session for device organization %d at %s; run 'wendy auth login' for that organization", device.GetOrganizationId(), device.GetCloudHost())
}

func registerAppsWithCloud(ctx context.Context, auth *config.AuthConfig, device *agentpb.ProvisionedResponse, appIDs []string) error {
	conn, err := dialCloudGRPC(auth)
	if err != nil {
		return err
	}
	defer conn.Close()
	cloudCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return err
	}
	client := cloudpb.NewAppServiceClient(conn)
	for _, id := range appIDs {
		// Keep operator-edited metadata and grants on an existing app. UpsertApp
		// is needed only when the catalog entry does not exist yet.
		app, err := client.GetApp(cloudCtx, &cloudpb.GetAppRequest{
			Id: id, OrganizationId: device.GetOrganizationId(),
		})
		if status.Code(err) == codes.NotFound {
			name := strings.TrimPrefix(id, "campaign:")
			app, err = client.UpsertApp(cloudCtx, &cloudpb.UpsertAppRequest{
				Id: id, OrganizationId: device.GetOrganizationId(), Name: &name,
			})
		}
		if err != nil {
			return fmt.Errorf("registering %s: %w", id, err)
		}
		if app.GetId() != id || app.GetOrganizationId() != device.GetOrganizationId() {
			return fmt.Errorf("Cloud returned a different app identity for %s", id)
		}
		cliLogln("Registered %s in Cloud Apps (organization %d).", id, device.GetOrganizationId())
		if !app.GetCanSendNotifications() {
			cliLogln("Notifications for %s are disabled; an owner or admin can enable its grant in the Cloud app settings.", id)
		}
	}
	return nil
}
