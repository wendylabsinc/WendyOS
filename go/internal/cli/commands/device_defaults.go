package commands

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
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
			if isLiteCloudAsset(asset) {
				return cloudLiteSelectedDevice(auth, asset, selector.String())
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

// saveDefaultDevice records key as the default device ("" clears it), changing
// only that field of the config as it is on disk now. Under config.Update, so
// an agent session setting its default cannot revert a login, pin or default
// another wendy process saved a moment earlier.
func saveDefaultDevice(key string) error {
	if err := config.Update(func(cfg *config.Config) (bool, error) {
		cfg.DefaultDevice = key
		return true, nil
	}); err != nil {
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
// an argument rather than through the command's --device flag. Like the CLI's
// direct path, it refuses an all-digit device (WDY-3126) before the TLS dial —
// but, like connectToAgentInner's own numeric check, only when
// WENDY_AGENT_SOCKET is unset: that variable already routes connectWithAutoTLS
// straight to the local unix socket regardless of device, so device is
// vestigial in that mode and must not be validated as a hostname.
func connectMCPDevice(ctx context.Context, device string) (*grpcclient.AgentConnection, error) {
	// A simulator connection can build and deploy its managed robot runtime.
	// Reserve stdout for JSON-RPC throughout that work, including warm deploys
	// that skip the build and only emit an upload heartbeat.
	ctx = context.WithValue(ctx, detachedJSONRunKey{}, true)
	if os.Getenv("WENDY_AGENT_SOCKET") == "" {
		selected, matched, err := connectNamedDeviceSelector(robotRuntimePromptContext(ctx, true), device, true)
		if err != nil {
			return nil, err
		}
		if matched {
			return selectedAgent(selected)
		}
		if err := rejectNumericDeviceName(device); err != nil {
			return nil, err
		}
	}
	// Dial a bare host as host:port, like the startup default device: that is
	// the form an mTLS device accepts here, and the target the run tool replays.
	return connectMCPDirectFn(ctx, mcpStartupAddress(device))
}

var connectMCPDirectFn = connectWithAutoTLS

// pickDeviceForDefaultFn is a seam so tests can prove the picker is never
// opened where it cannot work.
var pickDeviceForDefaultFn = pickDeviceForDefault

// maxSetDefaultCandidates bounds the device list a non-interactive
// set-default prints.
const maxSetDefaultCandidates = 10

// vmNamesFn lists the local VMs by name. A seam so tests describe the store.
var vmNamesFn = func() ([]string, error) {
	store, err := vm.NewStore()
	if err != nil {
		return nil, err
	}
	return store.List()
}

// setDefaultNeedsDeviceError is what `wendy device set-default` returns with no
// device where the picker cannot run: no terminal (a script, CI, an AI agent's
// shell) or --json. Bubbletea used to fail there with "could not open a new
// TTY", which says nothing about what to type instead.
func setDefaultNeedsDeviceError() error {
	var b strings.Builder
	b.WriteString("no device given, and the device picker needs an interactive terminal.\n")
	b.WriteString("Name the device to save as the default:\n  wendy device set-default <device>\n")
	if names := setDefaultCandidates(); len(names) > 0 {
		b.WriteString("Devices this CLI has seen recently:\n")
		for _, name := range names {
			fmt.Fprintf(&b, "  %s\n", name)
		}
	}
	b.WriteString("List everything reachable with 'wendy device list' ('wendy cloud discover' for cloud devices).")
	return commandErrorf(errNoDevice, "%s", b.String())
}

// setDefaultCandidates lists names set-default accepts, from local state only —
// the discovery cache and the VM store. It runs on an error path, so it never
// touches the network and treats any unreadable source as empty.
//
// Fresh, not Entries: the error text calls these "devices this CLI has seen
// recently", and Entries returns every cached entry regardless of age. Fresh
// bounds the list to the same TTL the picker and discovery use for display
// (Entries is reserved for the connect fast path, per its own doc comment).
func setDefaultCandidates() []string {
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		key := strings.ToLower(name)
		if name == "" || seen[key] {
			return
		}
		seen[key] = true
		names = append(names, name)
	}
	if cache, err := discoverycache.Load(); err == nil {
		for _, e := range cache.Fresh(time.Now()) {
			if e.Hostname != "" {
				add(e.Hostname)
			} else {
				add(e.IP)
			}
		}
	}
	if vms, err := vmNamesFn(); err == nil {
		for _, name := range vms {
			add(vmDeviceIDPrefix + name)
		}
	}
	sort.Strings(names)
	if len(names) > maxSetDefaultCandidates {
		names = names[:maxSetDefaultCandidates]
	}
	return names
}
