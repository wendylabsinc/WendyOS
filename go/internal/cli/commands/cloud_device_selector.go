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
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// A cloud selector names an asset in one organization and cloud, never a tunnel
// address or display name.
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

func cloudAssetSelector(auth *config.AuthConfig, asset *cloudpb.Asset) (string, error) {
	if auth == nil || auth.CloudGRPC == "" || cloudAuthOrgID(auth) <= 0 || asset.GetId() <= 0 {
		return "", fmt.Errorf("cloud selector requires a cloud endpoint, organization and asset ID")
	}
	key := (cloudDeviceSelector{Endpoint: auth.CloudGRPC, OrgID: cloudAuthOrgID(auth), AssetID: asset.GetId()}).String()
	_, _, err := parseCloudDeviceSelector(key)
	return key, err
}

func cloudDiscoverySelector(auth *config.AuthConfig, device cloudDiscoveryDevice) (string, error) {
	if device.legacy != nil {
		return cloudAssetSelector(auth, device.legacy)
	}
	if auth == nil || auth.CloudGRPC == "" || len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() == "" || device.v2.GetId() == "" {
		return "", fmt.Errorf("cloud selector requires a cloud endpoint, tenant and asset UUID")
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

// cloudAuthInOrganization reports whether auth belongs to a saved device's
// organization. A tenant UUID identifies one organization on its own. A numeric
// organization ID is unique only within its Cloud endpoint, and only a legacy
// session carries one in that namespace. Cloud selectors and device pins both
// select sessions with it.
func cloudAuthInOrganization(auth *config.AuthConfig, endpoint string, orgID int32, tenant string) bool {
	if auth == nil || len(auth.Certificates) == 0 {
		return false
	}
	authTenant := auth.Certificates[0].TenantUUID()
	if tenant != "" {
		return authTenant == tenant
	}
	return orgID > 0 && authTenant == "" && auth.CloudGRPC == endpoint && cloudAuthOrgID(auth) == orgID
}

// A selector also names its endpoint, which Cloud recorded when it was saved.
func (s cloudDeviceSelector) matches(auth *config.AuthConfig) bool {
	return auth != nil && auth.CloudGRPC == s.Endpoint && cloudAuthInOrganization(auth, s.Endpoint, s.OrgID, s.TenantUUID)
}

func (s cloudDeviceSelector) auth(cfg *config.Config) (*config.AuthConfig, error) {
	for i := range cfg.Auth {
		if s.matches(&cfg.Auth[i]) {
			return &cfg.Auth[i], nil
		}
	}
	return nil, s.missingAuth()
}

func (s cloudDeviceSelector) missingAuth() error {
	if s.TenantUUID != "" {
		return fmt.Errorf("no login for cloud %s tenant %s; log in to that tenant or choose another device", s.Endpoint, config.OrganizationLabel(s.Endpoint, s.TenantUUID))
	}
	return fmt.Errorf("no login for cloud %s organization %s; log in to that organization or choose another device", s.Endpoint, orgLabel(s.Endpoint, s.OrgID))
}

var fetchDefaultCloudAssetsFn = fetchCloudAssets
var connectDefaultCloudAssetFn = connectCloudAsset
var fetchDefaultCloudAssetsV2Fn = fetchCloudAssetsV2
var connectDefaultCloudAssetV2Fn = connectCloudAssetV2
var refreshDefaultCloudCertificateFn = refreshCertsForAuth

// A cloud selector resolves its own tenant session instead of going
// through pickAuthEntry. Prepare that session before discovery or the lazy
// tunnel dial can turn an expired certificate into an opaque PKI refusal.
func prepareDefaultCloudCertificate(ctx context.Context, auth *config.AuthConfig) error {
	if auth == nil || len(auth.Certificates) == 0 {
		return nil
	}
	// Renewal saves the whole config, even for non-OAuth sessions. Serialize
	// it with token rotation and reload before using or persisting credentials.
	unlock, err := acquireAuthRefreshLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := reloadOAuthSession(auth); err != nil {
		return err
	}
	err = ensureFreshCertificateFn(ctx, auth)
	if len(auth.Certificates) == 0 {
		return err
	}
	// Preflight suppresses transport failures. The certificate can expire
	// during that request, so check its current expiry even when err is nil.
	notAfter, expiryErr := leafNotAfterFn(auth.Certificates[0].PemCertificate)
	if expiryErr != nil {
		return err
	}
	if timeNowFn().Before(notAfter) {
		// Like pickAuthEntry, a refused renewal must not block a certificate
		// that can still authorize the connection.
		if err != nil {
			reportStaleCertificate(err)
		}
		return nil
	}
	if err == nil {
		err = errCertExpired
	}
	if auth.OAuthIssuer == "" || auth.ServiceAccount != "" {
		return err
	}
	// Keep the lock while an OAuth refresh token obtains a new certificate
	// after expiry, through persistence of any rotated token.
	err = refreshDefaultCloudCertificateFn(ctx, auth)
	// Issuance can fail after the refresh token rotates. Persist that token
	// even on failure so the next attempt can still use the selected session.
	if saveErr := persistOAuthSession(auth); err == nil {
		err = saveErr
	}
	return err
}

func connectCloudDeviceSelector(ctx context.Context, selector cloudDeviceSelector) (*SelectedDevice, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	auth, err := selector.auth(cfg)
	if err != nil {
		return nil, err
	}
	if err := prepareDefaultCloudCertificate(ctx, auth); err != nil {
		return nil, fmt.Errorf("preparing Cloud device certificate: %w", err)
	}
	// Certificate preparation reloads and can renew the selected session.
	if !selector.matches(auth) {
		return nil, selector.missingAuth()
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
			return &SelectedDevice{Agent: conn, Selector: selector.String()}, nil
		}
		return nil, fmt.Errorf("cloud asset %s in tenant %s is offline or unavailable; choose another device with --device", selector.AssetUUID, config.OrganizationLabel(selector.Endpoint, selector.TenantUUID))
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
		return &SelectedDevice{Agent: conn, Selector: selector.String()}, nil
	}
	return nil, fmt.Errorf("cloud asset %d in organization %s is offline or unavailable; choose another device with --device", selector.AssetID, orgLabel(selector.Endpoint, selector.OrgID))
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
	// Dial a bare host as host:port, like the MCP server's startup device: that is
	// the form an mTLS device accepts here, and the target the run tool replays.
	return connectMCPDirectFn(ctx, mcpStartupAddress(device))
}

var connectMCPDirectFn = connectWithAutoTLS
