package commands

import (
	"context"
	"errors"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudunenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
)

// DesktopManageDevice binds the confirmed action to the current device principal,
// organization and explicit saved operator account. It never chooses a default.
// Removing Cloud access deliberately does not reset the device's local identity.
func DesktopManageDevice(ctx context.Context, conn *grpcclient.AgentConnection, account, organization, device, name string, remove bool) error {
	if !remove {
		if err := validateHostnameArg(name); err != nil {
			return err
		}
	}
	if organization == "" || device == "" || account == "" {
		return errors.New("select a signed-in organization and refresh this device's identity first")
	}
	state, err := deviceProvisioning(ctx, conn)
	if err != nil {
		return err
	}
	prov := state.GetProvisioned()
	principal, err := certs.ParsePrincipal(prov.GetPrincipalUri())
	if err != nil || principal.EntityType != certs.EntityAsset || principal.TenantUUID != organization || principal.EntityID != device {
		return errors.New("the device enrollment changed or is unsupported; refresh its identity before continuing")
	}
	auth, err := desktopCloudAuth(account, organization)
	if err != nil {
		return err
	}
	if prov.GetCloudHost() == "" || auth.CloudGRPC != prov.GetCloudHost() {
		return errors.New("the selected account does not match the device's Cloud server")
	}
	err = withCloud(ctx, auth, func(ctx context.Context, cloud grpc.ClientConnInterface) error {
		asset, err := pb.NewAssetServiceClient(cloud).GetAsset(ctx, &pb.GetAssetRequest{OrganizationId: &organization, DeviceId: &device})
		if err != nil {
			return err
		}
		if asset.GetOrganizationId() != organization || cloudunenroll.DeviceID(asset) != device {
			return errors.New("Cloud returned a different device; no changes were made")
		}
		if remove {
			result, err := cloudunenroll.Remove(ctx, cloud, auth, asset)
			if err != nil {
				return fmt.Errorf("Cloud removal did not finish; credentials may already be revoked. Refresh Cloud before retrying: %w", err)
			}
			if !result.AssetDeleted {
				return errors.New("Cloud did not confirm device removal")
			}
			return nil
		}
		return cloudrequest.Invoke(ctx, cloud, auth, pb.AssetService_UpdateAsset_FullMethodName, &pb.UpdateAssetRequest{Id: asset.GetId(), Name: &name}, &pb.Asset{})
	})
	if err != nil {
		return err
	}
	if remove {
		return nil
	}
	if _, err := conn.AgentService.SetHostname(ctx, &agentpb.SetHostnameRequest{Hostname: name}); err != nil {
		return fmt.Errorf("Cloud name changed to %s, but the device hostname was not confirmed. Reconnect and retry the same name: %w", name, err)
	}
	return nil
}
