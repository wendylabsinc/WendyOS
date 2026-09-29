package commands

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// A cloud default names an asset in one organization and cloud, never a tunnel
// address or display name. Keep the existing string config format for LAN/VMs.
type cloudDeviceSelector struct {
	Endpoint   string
	OrgID      int32
	AssetID    int32
	TenantUUID string
	AssetUUID  string
}

func (s cloudDeviceSelector) String() string {
	if s.TenantUUID != "" {
		return (&url.URL{Scheme: "cloud", Host: s.Endpoint,
			Path: fmt.Sprintf("/tenant/%s/asset/%s", s.TenantUUID, s.AssetUUID)}).String()
	}
	return (&url.URL{Scheme: "cloud", Host: s.Endpoint,
		Path: fmt.Sprintf("/org/%d/asset/%d", s.OrgID, s.AssetID)}).String()
}

func cloudDeviceDefault(auth *config.AuthConfig, asset *cloudpb.Asset) (string, error) {
	if auth == nil || auth.CloudGRPC == "" || cloudAuthOrgID(auth) <= 0 || asset.GetId() <= 0 {
		return "", fmt.Errorf("cloud default requires a cloud endpoint, organization and asset ID")
	}
	key := (cloudDeviceSelector{Endpoint: auth.CloudGRPC, OrgID: cloudAuthOrgID(auth), AssetID: asset.GetId()}).String()
	_, _, err := parseCloudDeviceSelector(key)
	return key, err
}

func cloudDiscoveryDeviceDefault(auth *config.AuthConfig, device cloudDiscoveryDevice) (string, error) {
	if device.legacy != nil {
		return cloudDeviceDefault(auth, device.legacy)
	}
	if auth == nil || auth.CloudGRPC == "" || len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() == "" || device.v2.GetId() == "" {
		return "", fmt.Errorf("cloud default requires a cloud endpoint, tenant and asset UUID")
	}
	key := (cloudDeviceSelector{Endpoint: auth.CloudGRPC, TenantUUID: auth.Certificates[0].TenantUUID(), AssetUUID: device.v2.GetId()}).String()
	_, _, err := parseCloudDeviceSelector(key)
	return key, err
}

func parseCloudDeviceSelector(value string) (cloudDeviceSelector, bool, error) {
	var selector cloudDeviceSelector
	if !strings.HasPrefix(strings.ToLower(value), "cloud:") {
		return selector, false, nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return selector, true, fmt.Errorf("invalid cloud device selector; expected cloud://host:port/org/ID/asset/ID")
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) != 5 || parts[0] != "" || (parts[1] != "org" && parts[1] != "tenant") || parts[3] != "asset" {
		return selector, true, fmt.Errorf("invalid cloud device selector; expected /org/ID/asset/ID")
	}
	if parts[1] == "tenant" {
		tenant, tenantErr := uuid.Parse(parts[2])
		asset, assetErr := uuid.Parse(parts[4])
		if tenantErr != nil || assetErr != nil || tenant == uuid.Nil || asset == uuid.Nil {
			return selector, true, fmt.Errorf("cloud tenant and asset IDs must be nonzero UUIDs")
		}
		return cloudDeviceSelector{Endpoint: u.Host, TenantUUID: tenant.String(), AssetUUID: asset.String()}, true, nil
	}
	org, orgErr := strconv.ParseInt(parts[2], 10, 32)
	asset, assetErr := strconv.ParseInt(parts[4], 10, 32)
	if orgErr != nil || assetErr != nil || org <= 0 || asset <= 0 {
		return selector, true, fmt.Errorf("cloud organization and asset IDs must be positive integers")
	}
	return cloudDeviceSelector{Endpoint: u.Host, OrgID: int32(org), AssetID: int32(asset)}, true, nil
}

func (s cloudDeviceSelector) auth(cfg *config.Config) (*config.AuthConfig, error) {
	for i := range cfg.Auth {
		auth := &cfg.Auth[i]
		if auth.CloudGRPC != s.Endpoint {
			continue
		}
		if s.TenantUUID != "" {
			if len(auth.Certificates) > 0 && auth.Certificates[0].TenantUUID() == s.TenantUUID {
				return auth, nil
			}
			continue
		}
		if cloudAuthOrgID(auth) == s.OrgID {
			return auth, nil
		}
	}
	if s.TenantUUID != "" {
		return nil, fmt.Errorf("no login for cloud %s tenant %s; log in to that tenant or choose another default", s.Endpoint, s.TenantUUID)
	}
	return nil, fmt.Errorf("no login for cloud %s organization %d; log in to that organization or choose another default", s.Endpoint, s.OrgID)
}

var fetchDefaultCloudAssetsFn = fetchCloudAssets
var connectDefaultCloudAssetFn = connectCloudAsset
var fetchDefaultCloudAssetsV2Fn = fetchCloudAssetsV2
var connectDefaultCloudAssetV2Fn = connectCloudAssetV2

func connectCloudDeviceSelector(ctx context.Context, selector cloudDeviceSelector) (*SelectedDevice, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	auth, err := selector.auth(cfg)
	if err != nil {
		return nil, err
	}
	if selector.TenantUUID != "" {
		assets, err := fetchDefaultCloudAssetsV2Fn(ctx, auth, true)
		if err != nil {
			return nil, err
		}
		for _, asset := range assets {
			if asset.GetId() != selector.AssetUUID {
				continue
			}
			conn, err := connectDefaultCloudAssetV2Fn(ctx, auth, asset, os.Getenv("WENDY_BROKER_URL"))
			if err != nil {
				return nil, err
			}
			return &SelectedDevice{Agent: conn, DefaultSelector: selector.String()}, nil
		}
		return nil, fmt.Errorf("cloud asset %s in tenant %s is offline or unavailable; choose another device with --device or 'wendy device set-default'", selector.AssetUUID, selector.TenantUUID)
	}
	assets, err := fetchDefaultCloudAssetsFn(ctx, auth)
	if err != nil {
		return nil, err
	}
	for _, asset := range assets {
		if asset.GetId() != selector.AssetID {
			continue
		}
		conn, err := connectDefaultCloudAssetFn(ctx, auth, asset, os.Getenv("WENDY_BROKER_URL"))
		if err != nil {
			return nil, err
		}
		return &SelectedDevice{Agent: conn, DefaultSelector: selector.String()}, nil
	}
	return nil, fmt.Errorf("cloud asset %d in organization %d is offline or unavailable; choose another device with --device or 'wendy device set-default'", selector.AssetID, selector.OrgID)
}

func saveDefaultDevice(key string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading default device: %w", err)
	}
	cfg.DefaultDevice = key
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("saving default device: %w", err)
	}
	return nil
}

func setPickerDefault(item tui.PickerItem) (string, error) {
	key := pickerItemDeviceID(item)
	if choice, ok := item.Value.(*simulatorChoice); ok && choice != nil {
		if choice.Create {
			return "", fmt.Errorf("create the simulator before setting it as default")
		}
		key = vmDeviceIDPrefix + choice.Name
	}
	if key == "" {
		return "", fmt.Errorf("this row has no persistent device identity")
	}
	if err := saveDefaultDevice(key); err != nil {
		return "", err
	}
	return fmt.Sprintf("Default device set to %s.", item.Name), nil
}

func unsetPickerDefault() (string, error) {
	if err := saveDefaultDevice(""); err != nil {
		return "", err
	}
	return "Default device cleared.", nil
}

// Keep a narrow seam for front-door routing tests without starting a real VM
// or opening a cloud tunnel.
var connectCloudDeviceSelectorFn = connectCloudDeviceSelector

func connectNamedDeviceSelector(ctx context.Context, device string, suppressUpdate bool) (*SelectedDevice, bool, error) {
	if selector, matched, err := parseCloudDeviceSelector(device); matched {
		if err != nil {
			return nil, true, err
		}
		picked, err := connectCloudDeviceSelectorFn(ctx, selector)
		return picked, true, err
	}
	if name, matched, err := simulatorName(device); matched || err != nil {
		if err != nil {
			return nil, true, err
		}
		picked, err := connectSimulatorChoiceFn(ctx, &simulatorChoice{Name: name}, suppressUpdate)
		return picked, true, err
	}
	return nil, false, nil
}

// Used by the MCP connection adapter, which receives the selected identity as
// an argument rather than through the command's --device flag.
func connectMCPDevice(ctx context.Context, device string) (*grpcclient.AgentConnection, error) {
	if os.Getenv("WENDY_AGENT_SOCKET") == "" {
		selected, matched, err := connectNamedDeviceSelector(robotRuntimePromptContext(ctx, true), device, true)
		if err != nil {
			return nil, err
		}
		if matched {
			return selected.Agent, nil
		}
	}
	// Dial a bare host as host:port, like the startup default device: that is
	// the form an mTLS device accepts here, and the target the run tool replays.
	return connectMCPDirectFn(ctx, mcpStartupAddress(device))
}

var connectMCPDirectFn = connectWithAutoTLS
