package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/wendylabsinc/wendy-go/cloud"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func int32Ptr(v int32) *int32 { return &v }

// mcpListCloudAssetsV2 lists compute-device assets on the v2 AssetService,
// tenant-scoped by the session's SPIFFE principal. The v1 mcpListCloudAssets
// sends cert.OrganizationID, which is 0 for a v2 (UUID-identity) session, so it
// silently queried org 0 and returned nothing (WDY-3146). WendyGo now owns
// the same complete inventory operation used by commands/fetchCloudAssetsV2.
func mcpListCloudAssetsV2(ctx context.Context, auth *config.AuthConfig, filter string, onlineOnly bool) ([]*cloudpbv2.Asset, error) {
	identity, err := certs.ParsePrincipal(auth.Certificates[0].PrincipalURI)
	if err != nil {
		return nil, fmt.Errorf("reading cloud organization: %w", err)
	}
	conn, err := mcpDialCloudGRPC(auth)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	client, err := cloudrequest.Client(conn, func(ctx context.Context) (context.Context, error) { return mcpCloudContext(ctx, auth) })
	if err != nil {
		return nil, err
	}
	assets, err := client.ListDevices(ctx, identity.TenantUUID, filter, onlineOnly)
	if err != nil {
		var limit *cloud.DeviceLimitError
		if errors.As(err, &limit) {
			return nil, &cloudResolveErr{code: errCodeInvalidArgument, msg: err.Error()}
		}
		return nil, err
	}
	return cloudrequest.Assets(assets)
}

// cloudAssetV2ToMap renders a v2 asset for the MCP discover tool. Per sem's
// "device ID, not asset ID" rule the identity emitted is the device id (the v2
// asset's UUID), labelled device_id, alongside device_name — never an int32
// asset id.
func cloudAssetV2ToMap(a *cloudpbv2.Asset) map[string]any {
	out := map[string]any{
		"device_id":         a.GetId(),
		"device_name":       a.GetName(),
		"organization_id":   a.GetOrganizationId(),
		"asset_type":        a.GetAssetType(),
		"is_compute_device": a.GetIsComputeDevice(),
	}
	if a.DeviceType != nil {
		out["device_type"] = a.GetDeviceType()
	}
	if a.Architecture != nil {
		out["architecture"] = a.GetArchitecture()
	}
	if a.OsVersion != nil {
		out["os_version"] = a.GetOsVersion()
	}
	return out
}
