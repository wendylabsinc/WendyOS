package commands

import (
	"context"
	"errors"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"io"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// v2 uses UUIDs. Keep them intact in JSON instead of converting them into the
// numeric identifiers used by legacy sessions.
type cloudDiscoverV2Info struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Address string `json:"address"`
	Version string `json:"version,omitempty"`
}

func fetchCloudAssetsV2(ctx context.Context, auth *config.AuthConfig, onlineOnly bool) ([]*cloudpbv2.Asset, error) {
	identity, err := certs.ParsePrincipal(auth.Certificates[0].PrincipalURI)
	if err != nil {
		return nil, fmt.Errorf("reading cloud organization: %w", err)
	}
	conn, err := dialCloudGRPC(auth)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	client := cloudpbv2.NewAssetServiceClient(conn)
	assets := make([]*cloudpbv2.Asset, 0)
	const pageSize = 200
	for offset := int32(0); ; {
		req := &cloudpbv2.ListAssetsRequest{
			OrganizationId:  identity.TenantUUID,
			IsComputeDevice: boolPtr(true),
			Offset:          int32Ptr(offset),
			Limit:           int32Ptr(pageSize),
		}
		if onlineOnly {
			req.OnlineOnly = boolPtr(true)
		}
		cloudCtx, err := cloudContext(ctx, auth)
		if err != nil {
			return nil, err
		}
		stream, err := client.ListAssets(cloudCtx, req)
		if err != nil {
			return nil, fmt.Errorf("listing devices: %w", err)
		}
		var page, total int32
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("listing devices: %w", err)
			}
			if len(assets) >= maxCloudAssets {
				return nil, fmt.Errorf("cloud returned more than %d devices", maxCloudAssets)
			}
			assets = append(assets, resp.GetAsset())
			page++
			total = resp.GetTotal()
		}
		offset += page
		if page == 0 || offset >= total {
			return assets, nil
		}
	}
}

// cloudDiscoveryDevice preserves each API's identity while sharing the display,
// clipboard, version-probe and update paths across both generations.
type cloudDiscoveryDevice struct {
	cloudAssetMetadata
	legacy *cloudpb.Asset
	v2     *cloudpbv2.Asset
	key    string
}

type cloudAssetMetadata interface {
	GetName() string
	GetDeviceType() string
	GetOsType() string
	GetArchitecture() string
	GetIpAddress() string
}

func legacyDiscoveryDevices(assets []*cloudpb.Asset) []cloudDiscoveryDevice {
	devices := make([]cloudDiscoveryDevice, 0, len(assets))
	for _, a := range assets {
		devices = append(devices, cloudDiscoveryDevice{cloudAssetMetadata: a, legacy: a, key: fmt.Sprint(a.GetId())})
	}
	return devices
}

func fetchCloudDiscoveryDevices(ctx context.Context, auth *config.AuthConfig, onlineOnly bool) ([]cloudDiscoveryDevice, error) {
	if len(auth.Certificates) == 0 || auth.Certificates[0].PrincipalURI == "" {
		assets, err := fetchCloudAssetsFiltered(ctx, auth, onlineOnly)
		if err != nil {
			return nil, err
		}
		seedPinsFromAssetsBestEffort(auth, assets)
		return legacyDiscoveryDevices(assets), nil
	}
	assets, err := fetchCloudAssetsV2(ctx, auth, onlineOnly)
	if err != nil {
		return nil, err
	}
	devices := make([]cloudDiscoveryDevice, 0, len(assets))
	for _, a := range assets {
		devices = append(devices, cloudDiscoveryDevice{cloudAssetMetadata: a, v2: a, key: a.GetId()})
	}
	return devices, nil
}

func (d cloudDiscoveryDevice) info(ver *agentpb.GetAgentVersionResponse) any {
	if d.legacy != nil {
		return cloudDeviceInfoFromAsset(d.legacy, ver)
	}
	deviceType := humanReadableDeviceType(d.GetDeviceType())
	if deviceType == "" {
		deviceType = humanReadableOSType(d.GetOsType(), d.GetArchitecture())
	}
	info := cloudDiscoverV2Info{ID: d.key, Name: d.GetName(), Type: deviceType, Address: d.GetIpAddress()}
	if ver != nil {
		if info.Type == "" {
			info.Type = humanReadableDeviceType(ver.GetDeviceType())
		}
		if info.Type == "" {
			info.Type = humanReadableOSType(ver.GetOs(), ver.GetCpuArchitecture())
		}
		info.Version = ver.GetVersion()
	}
	return info
}

func (d cloudDiscoveryDevice) connect(ctx context.Context, auth *config.AuthConfig, brokerURL string) (*grpcclient.AgentConnection, error) {
	if d.legacy != nil {
		return connectCloudAsset(ctx, auth, d.legacy, brokerURL)
	}
	return connectCloudAssetV2(ctx, auth, d.v2, brokerURL)
}

func (d cloudDiscoveryDevice) reconnect(ctx context.Context, auth *config.AuthConfig, brokerURL string) (*grpcclient.AgentConnection, error) {
	return waitForCloudDeviceRestart(ctx, d.GetName(), d.key, func(ctx context.Context) (*grpcclient.AgentConnection, error) { return d.connect(ctx, auth, brokerURL) })
}

// pickCloudDiscoveryDevice keeps UUID identities intact through selection.
func pickCloudDiscoveryDevice(ctx context.Context, auth *config.AuthConfig, name, brokerURL string) (cloudDiscoveryDevice, error) {
	if len(auth.Certificates) == 0 {
		return cloudDiscoveryDevice{}, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
	}
	if auth.Certificates[0].PrincipalURI == "" {
		a, err := pickCloudDevice(ctx, auth, name, brokerURL)
		if err != nil {
			return cloudDiscoveryDevice{}, err
		}
		return legacyDiscoveryDevices([]*cloudpb.Asset{a})[0], nil
	}
	devices, err := fetchCloudDiscoveryDevices(ctx, auth, true)
	if err != nil {
		return cloudDiscoveryDevice{}, err
	}
	if name == "" && len(devices) > 1 && isInteractiveTerminal() {
		m := newCloudDiscoverModel(ctx, auth, brokerURL, false, true, nil)
		m.devices = devices
		m.hasResults = true
		m.refreshTable()
		final, err := tea.NewProgram(m).Run()
		if err != nil {
			return cloudDiscoveryDevice{}, err
		}
		picked := final.(cloudDiscoverModel)
		if picked.selectedV2 == nil {
			return cloudDiscoveryDevice{}, ErrUserCancelled
		}
		a := picked.selectedV2
		return cloudDiscoveryDevice{cloudAssetMetadata: a, v2: a, key: a.GetId()}, nil
	}
	d, err := resolveCloudDiscoveryDevice(devices, name)
	if err == nil {
		if name == "" {
			noteImplicitDevice(d.GetName(), implicitSoleCloudDevice)
		}
		return d, nil
	}
	var missing *errCloudDeviceNotFound
	if errors.Is(err, errNoCloudDevicesEnrolled) || errors.As(err, &missing) {
		all, fetchErr := fetchCloudDiscoveryDevices(ctx, auth, false)
		if fetchErr == nil {
			if name == "" && len(all) > 0 {
				return cloudDiscoveryDevice{}, fmt.Errorf("all %d enrolled devices are currently reported offline; check the agents' Cloud presence logs ('wendy cloud discover --all --json' lists enrolled devices)", len(all))
			}
			if _, e := resolveCloudDiscoveryDevice(all, name); e == nil {
				return cloudDiscoveryDevice{}, fmt.Errorf("device %q is enrolled but currently reported offline; check its Cloud presence logs", name)
			}
		}
	}
	return cloudDiscoveryDevice{}, err
}
func resolveCloudDiscoveryDevice(devices []cloudDiscoveryDevice, name string) (cloudDiscoveryDevice, error) {
	if len(devices) == 0 {
		if name != "" {
			return cloudDiscoveryDevice{}, &errCloudDeviceNotFound{name: name}
		}
		return cloudDiscoveryDevice{}, errNoCloudDevicesEnrolled
	}
	if name != "" {
		// Prefer a UUID match, so duplicate display names never hide an exact ID.
		for _, d := range devices {
			if strings.EqualFold(d.key, strings.TrimSpace(name)) {
				return d, nil
			}
		}
		var match *cloudDiscoveryDevice
		for i := range devices {
			if strings.EqualFold(devices[i].GetName(), name) {
				if match != nil {
					return cloudDiscoveryDevice{}, fmt.Errorf("multiple devices match %q; use a device UUID", name)
				}
				match = &devices[i]
			}
		}
		if match != nil {
			return *match, nil
		}
		return cloudDiscoveryDevice{}, &errCloudDeviceNotFound{name: name}
	}
	if len(devices) == 1 {
		return devices[0], nil
	}
	var names []string
	for _, d := range devices {
		names = append(names, d.key+"="+d.GetName())
	}
	return cloudDiscoveryDevice{}, fmt.Errorf("multiple cloud devices found; rerun with --device <id|name> (%s)", strings.Join(names, ", "))
}
