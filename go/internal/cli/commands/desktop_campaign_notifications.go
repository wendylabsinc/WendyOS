package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	legacy "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// DesktopEnableCampaignNotifications is called only after the native review is
// accepted. The grant belongs to the organization's campaign app, not one device.
func DesktopEnableCampaignNotifications(ctx context.Context, conn *grpcclient.AgentConnection, name string) error {
	return DesktopSetCampaignNotifications(ctx, conn, name, true)
}

func DesktopSetCampaignNotifications(ctx context.Context, conn *grpcclient.AgentConnection, name string, enabled bool) error {
	if name == "" || name != strings.TrimSpace(name) || len(name) > 80 {
		return errors.New("choose an existing campaign")
	}
	if conn == nil || conn.DataService == nil {
		return errors.New("reconnect the original chat device")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	campaigns, err := conn.DataService.Campaigns(ctx, &agentpb.DataCampaignsRequest{})
	if err != nil {
		return err
	}
	found := false
	for _, campaign := range campaigns.GetCampaigns() {
		if campaign.GetName() != name {
			continue
		}
		var plan data.Campaign
		if err := json.Unmarshal(campaign.GetPlanJson(), &plan); err != nil {
			return errors.New("cannot read the campaign's notification settings")
		}
		if enabled && plan.Notify == nil {
			return errors.New("this campaign has notifications disabled; enable notifications in its settings first")
		}
		found = true
	}
	if !found {
		return errors.New("campaign no longer exists on this device")
	}
	// An existing campaign may still have the old colon-based Cloud identity.
	// Register its canonical runtime identity only after the user's grant approval.
	if enabled {
		if err := registerCloudApps(ctx, conn, []string{data.CampaignAppID(name)}, false); err != nil {
			return err
		}
	}
	return withCampaignCloud(ctx, conn, func(ctx context.Context, cloud grpc.ClientConnInterface, auth *config.AuthConfig, device *agentpb.ProvisionedResponse) error {
		return setCampaignCloudGrant(ctx, cloud, auth, device, data.CampaignAppID(name), enabled)
	})
}

func withCampaignCloud(ctx context.Context, conn *grpcclient.AgentConnection, action func(context.Context, grpc.ClientConnInterface, *config.AuthConfig, *agentpb.ProvisionedResponse) error) error {
	response, err := (deploymentProvisioningService{conn}).IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
	if err != nil {
		return err
	}
	device := response.GetProvisioned()
	if device == nil {
		return errors.New("this device must be enrolled in Wendy Cloud before enabling notifications")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	auth, err := deploymentAuth(cfg, device)
	if err != nil {
		return err
	}
	cloud, err := dialCloudGRPC(auth)
	if err != nil {
		return err
	}
	defer cloud.Close()
	cloudCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return err
	}
	return action(cloudCtx, cloud, auth, device)
}

func enableCampaignCloudGrant(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, device *agentpb.ProvisionedResponse, id string) error {
	return setCampaignCloudGrant(ctx, conn, auth, device, id, true)
}
func setCampaignCloudGrant(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, device *agentpb.ProvisionedResponse, id string, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if device.GetPrincipalUri() != "" {
		identity, err := certs.ParsePrincipal(device.GetPrincipalUri())
		if err != nil || identity.EntityType != certs.EntityAsset || identity.TenantUUID == "" {
			return errors.New("invalid device enrollment")
		}
		tenant := identity.TenantUUID
		client := cloudpb.NewAppServiceClient(conn)
		app, err := client.GetApp(ctx, &cloudpb.GetAppRequest{Id: id, OrganizationId: tenant})
		if err != nil {
			return fmt.Errorf("reading campaign notification permission: %w", err)
		}
		if app.GetId() != id || app.GetOrganizationId() != tenant {
			return errors.New("Cloud returned a different campaign identity")
		}
		if app.GetCanSendNotifications() == enabled {
			return nil
		}
		updated := new(cloudpb.App)
		err = cloudrequest.Invoke(ctx, conn, auth, cloudpb.AppService_UpdateApp_FullMethodName,
			&cloudpb.UpdateAppRequest{Id: id, OrganizationId: tenant, CanSendNotifications: proto.Bool(enabled)}, updated)
		if err != nil {
			return fmt.Errorf("updating campaign notifications: %w", err)
		}
		if updated.GetId() != id || updated.GetOrganizationId() != tenant || updated.GetCanSendNotifications() != enabled {
			return errors.New("Cloud did not confirm the requested notification permission")
		}
		return nil
	}
	org := device.GetOrganizationId()
	if org <= 0 {
		return errors.New("invalid device organization")
	}
	client := legacy.NewAppServiceClient(conn)
	app, err := client.GetApp(ctx, &legacy.GetAppRequest{Id: id, OrganizationId: org})
	if err != nil {
		return fmt.Errorf("reading campaign notification permission: %w", err)
	}
	if app.GetId() != id || app.GetOrganizationId() != org {
		return errors.New("Cloud returned a different campaign identity")
	}
	if app.GetCanSendNotifications() == enabled {
		return nil
	}
	updated, err := client.UpdateApp(ctx, &legacy.UpdateAppRequest{Id: id, OrganizationId: org, CanSendNotifications: proto.Bool(enabled)})
	if err != nil {
		return fmt.Errorf("updating campaign notifications: %w", err)
	}
	if updated.GetId() != id || updated.GetOrganizationId() != org || updated.GetCanSendNotifications() != enabled {
		return errors.New("Cloud did not confirm the requested notification permission")
	}
	return nil
}

type DesktopCampaignNotification struct {
	Name                                       string
	Enabled, Registered, RequestsNotifications bool
}

func DesktopCampaignNotifications(ctx context.Context, conn *grpcclient.AgentConnection) ([]DesktopCampaignNotification, error) {
	if conn == nil || conn.DataService == nil {
		return nil, errors.New("select a device first")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	campaigns, err := conn.DataService.Campaigns(ctx, &agentpb.DataCampaignsRequest{})
	if err != nil {
		return nil, err
	}
	rows := make([]DesktopCampaignNotification, 0, len(campaigns.GetCampaigns()))
	for _, campaign := range campaigns.GetCampaigns() {
		var plan data.Campaign
		if err := json.Unmarshal(campaign.GetPlanJson(), &plan); err != nil {
			return nil, errors.New("cannot read campaign notification settings")
		}
		rows = append(rows, DesktopCampaignNotification{Name: campaign.GetName(), RequestsNotifications: plan.Notify != nil})
	}
	if len(rows) == 0 {
		return rows, nil
	}
	err = withCampaignCloud(ctx, conn, func(ctx context.Context, cloud grpc.ClientConnInterface, auth *config.AuthConfig, device *agentpb.ProvisionedResponse) error {
		for i := range rows {
			id := data.CampaignAppID(rows[i].Name)
			if device.GetPrincipalUri() != "" {
				identity, err := certs.ParsePrincipal(device.GetPrincipalUri())
				if err != nil || identity.EntityType != certs.EntityAsset || identity.TenantUUID == "" {
					return errors.New("invalid device enrollment")
				}
				app, err := cloudpb.NewAppServiceClient(cloud).GetApp(ctx, &cloudpb.GetAppRequest{Id: id, OrganizationId: identity.TenantUUID})
				if status.Code(err) == codes.NotFound {
					continue
				}
				if err != nil {
					return err
				}
				if app.GetId() != id || app.GetOrganizationId() != identity.TenantUUID {
					return errors.New("Cloud returned a different campaign identity")
				}
				rows[i].Enabled, rows[i].Registered = app.GetCanSendNotifications(), true
			} else {
				app, err := legacy.NewAppServiceClient(cloud).GetApp(ctx, &legacy.GetAppRequest{Id: id, OrganizationId: device.GetOrganizationId()})
				if status.Code(err) == codes.NotFound {
					continue
				}
				if err != nil {
					return err
				}
				if app.GetId() != id || app.GetOrganizationId() != device.GetOrganizationId() {
					return errors.New("Cloud returned a different campaign identity")
				}
				rows[i].Enabled, rows[i].Registered = app.GetCanSendNotifications(), true
			}
		}
		return nil
	})
	return rows, err
}
