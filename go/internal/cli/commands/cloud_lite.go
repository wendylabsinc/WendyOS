package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
)

// liteCloudOSType is the os_type Cloud stamps on an asset once its board's
// WendyCom link is up. Such a board has no WendyOS agent to tunnel to: it is
// reached through Cloud's WendyCom relay instead.
const liteCloudOSType = "wendy-lite"

func init() {
	providers.CloudRelayDialer = dialLiteCloudRelay
}

// isLiteCloudAsset reports whether Cloud lists asset as a Wendy Lite board.
func isLiteCloudAsset(asset *cloudpbv2.Asset) bool {
	return strings.EqualFold(asset.GetOsType(), liteCloudOSType)
}

// liteDeviceType labels a Lite board's type in Cloud listings the way the
// local tab does ("LAN (Lite)").
func liteDeviceType(devType string) string {
	if devType == "" {
		return "Lite"
	}
	return devType + " (Lite)"
}

// cloudLiteSelectedDevice targets a Wendy Lite asset through the Cloud relay.
// Nothing is dialled here: like its other transports, the Lite provider
// connects when a command uses the device. selector is the asset's cloud://
// default.
func cloudLiteSelectedDevice(auth *config.AuthConfig, asset *cloudpbv2.Asset, selector string) (*SelectedDevice, error) {
	if auth == nil || auth.CloudGRPC == "" || len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() == "" || asset.GetId() == "" {
		return nil, fmt.Errorf("a Wendy Lite cloud device requires a cloud endpoint, tenant and asset UUID")
	}
	provider := &providers.MicroWendyProvider{}
	return &SelectedDevice{
		External: &models.ExternalDevice{
			ID:          "wendy-lite:cloud:" + asset.GetId(),
			DisplayName: asset.GetName(),
			ProviderKey: provider.Key(),
			ConnectionInfo: map[string]string{
				"type":     "Cloud",
				"assetId":  asset.GetId(),
				"cloud":    auth.CloudGRPC,
				"tenantId": auth.Certificates[0].TenantUUID(),
				"deviceId": asset.GetPkiDeviceName(),
			},
			IsWendyDevice:   true,
			OS:              asset.GetOsType(),
			OSVersion:       asset.GetOsVersion(),
			CPUArchitecture: asset.GetArchitecture(),
		},
		Provider:        provider,
		DefaultSelector: selector,
	}, nil
}

// selectedAgent is the agent of a named target, for callers that need one. A
// Wendy Lite board picked through Cloud has none, so they refuse it rather
// than pass on a nil connection.
func selectedAgent(picked *SelectedDevice) (*grpcclient.AgentConnection, error) {
	if picked.Agent != nil {
		return picked.Agent, nil
	}
	if picked.External != nil {
		return nil, fmt.Errorf("%s is a Wendy Lite device; this command needs a WendyOS agent", picked.External.DisplayName)
	}
	return nil, fmt.Errorf("selected device has no WendyOS agent")
}

// dialLiteCloudRelay is the Lite provider's providers.CloudRelayDialer. It
// uses the login for the device's cloud and tenant, as a cloud:// selector
// does, and the same connection and call metadata as every other Cloud RPC,
// so the relay sees the user's session (DPoP when the token is key-bound).
func dialLiteCloudRelay(ctx context.Context, device models.ExternalDevice) (*grpc.ClientConn, context.Context, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	selector := cloudDeviceSelector{Endpoint: device.ConnectionInfo["cloud"], TenantUUID: device.ConnectionInfo["tenantId"]}
	if selector.Endpoint == "" || selector.TenantUUID == "" {
		return nil, nil, fmt.Errorf("Wendy Lite cloud device has no cloud endpoint or tenant")
	}
	auth, err := selector.auth(cfg)
	if err != nil {
		return nil, nil, err
	}
	conn, err := dialCloudGRPC(auth)
	if err != nil {
		return nil, nil, err
	}
	cloudCtx, err := cloudContext(ctx, auth)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, cloudCtx, nil
}
