package commands

import (
	"context"

	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// Reuse the version-aware Cloud inventory and exact identity matching used by
// ordinary device selection. A changed CLI default never redirects a source.
func discoverGatewayCloud(ctx context.Context, source wendymcp.GatewayCloudSource, onlineOnly bool) ([]wendymcp.GatewayCloudDevice, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	auth, err := (cloudDeviceSelector{Endpoint: source.Endpoint, OrgID: source.OrganizationID, TenantUUID: source.TenantUUID}).auth(cfg)
	if err != nil {
		return nil, err
	}
	return gatewayCloudInventory(ctx, auth, onlineOnly)
}

func gatewayCloudInventory(ctx context.Context, auth *config.AuthConfig, onlineOnly bool) ([]wendymcp.GatewayCloudDevice, error) {
	devices, err := fetchCloudDiscoveryDevices(ctx, auth, onlineOnly)
	if err != nil {
		return nil, err
	}
	rows := make([]wendymcp.GatewayCloudDevice, 0, len(devices))
	for _, device := range devices {
		selector, err := cloudDiscoveryDeviceDefault(auth, device)
		if err != nil {
			return nil, err
		}
		deviceType := humanReadableDeviceType(device.GetDeviceType())
		if deviceType == "" {
			deviceType = humanReadableOSType(device.GetOsType(), device.GetArchitecture())
		}
		rows = append(rows, wendymcp.GatewayCloudDevice{Device: selector, Name: device.GetName(), Type: deviceType})
	}
	return rows, nil
}
