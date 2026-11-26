package commands

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshname"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const privateAliasDiscoveryTimeout = 3 * time.Second
const privateAliasLANHeadStart = 500 * time.Millisecond
const privateAliasMeshDiscoveryTimeout = 15 * time.Second
const privateAliasMeshProbeTimeout = 15 * time.Second
const privateAliasScopePrefix = "private-alias/"

type privateDeviceAlias struct {
	host   string
	label  string
	uuid   string
	tenant meshid.Tenant
}

func (a privateDeviceAlias) pinKey(tenant meshid.Tenant) string {
	return privateAliasScopePrefix + tenant.String() + "/" + a.host
}

func privateAliasPinIdentity(cfg *config.Config, key string) (*certs.WendyIdentity, bool) {
	pin, ok := cfg.DevicePinFor(key)
	if !ok {
		return nil, false
	}
	return expectedIdentityForPin(pin), true
}

func (a privateDeviceAlias) expectedIdentity(cfg *config.Config, tenant meshid.Tenant) (*certs.WendyIdentity, error) {
	if identity, found := privateAliasPinIdentity(cfg, a.pinKey(tenant)); found {
		if identity == nil || identity.EntityType != certs.EntityAsset || identity.TenantUUID != tenant.String() {
			return nil, refuseIdentity("private device alias %q has an invalid saved binding for the selected organization", a.host)
		}
		return identity, nil
	}
	// An older full alias can seed the scoped pin. A bare name can also
	// come from ordinary LAN discovery and has no Cloud-name provenance.
	identity, _ := privateAliasPinIdentity(cfg, a.host)
	if identity != nil && identity.EntityType == certs.EntityAsset && identity.TenantUUID == tenant.String() {
		return identity, nil
	}
	return nil, nil
}

// Private device aliases select an enrolled identity, rather than asking the
// operating system to resolve the private suffix. In particular, no desktop VPN
// is necessary for CLI selection. The previous private namespace keeps its
// existing resolution behavior.
func parsePrivateDeviceAlias(value string) (privateDeviceAlias, bool, error) {
	if scoped, found := strings.CutPrefix(value, privateAliasScopePrefix); found {
		tenantText, host, hasHost := strings.Cut(scoped, "/")
		tenant, err := meshid.ParseTenant(tenantText)
		if !hasHost || strings.Contains(host, "/") || err != nil {
			return privateDeviceAlias{}, true, fmt.Errorf("invalid scoped private device alias %q", value)
		}
		alias, matched, err := parsePrivateDeviceAlias(host)
		if err != nil {
			return privateDeviceAlias{}, true, err
		}
		if !matched {
			return privateDeviceAlias{}, true, fmt.Errorf("invalid scoped private device alias %q", value)
		}
		alias.tenant = tenant
		return alias, true, nil
	}
	host := strings.ToLower(strings.TrimSuffix(value, "."))
	if bare, _, err := net.SplitHostPort(value); err == nil {
		parsed, matched, parseErr := parsePrivateDeviceAlias(bare)
		if matched {
			if parseErr != nil {
				return parsed, true, parseErr
			}
			return parsed, true, fmt.Errorf("private device alias %q selects an enrolled agent; omit the explicit port", value)
		}
	}
	if strings.HasSuffix(host, "."+meshname.Suffix) {
		return privateDeviceAlias{}, false, nil
	}
	if !strings.HasSuffix(host, "."+meshname.ShortSuffix) && host != meshname.ShortSuffix {
		return privateDeviceAlias{}, false, nil
	}
	alias := privateDeviceAlias{host: host, label: strings.TrimSuffix(host, "."+meshname.ShortSuffix)}
	for _, c := range value {
		if c > 127 {
			return alias, true, fmt.Errorf("invalid private device alias %q: ASCII DNS labels are required", value)
		}
	}
	if strings.HasPrefix(alias.label, "device-") {
		if id, err := meshid.ParseDeviceUUID(strings.TrimPrefix(alias.label, "device-")); err == nil {
			alias.uuid = id.String()
			return alias, true, nil
		}
	}
	label, ok := meshname.FriendlyLabel(alias.label)
	if !ok || host == meshname.ShortSuffix {
		return alias, true, fmt.Errorf("invalid private device alias %q", value)
	}
	alias.label = label
	return alias, true, nil
}

func (a privateDeviceAlias) authScope(cfg *config.Config) (*config.AuthConfig, meshid.Tenant, error) {
	auth, err := config.ResolveAuth(cfg, "", nil)
	if err != nil {
		return nil, meshid.Tenant{}, err
	}
	if len(auth.Certificates) == 0 {
		return nil, meshid.Tenant{}, certs.ErrExpectedDeviceIdentity
	}
	tenant, err := meshid.ParseTenant(auth.Certificates[0].TenantUUID())
	if err != nil {
		return nil, meshid.Tenant{}, fmt.Errorf("private device aliases require a selected enrolled organization: %w", err)
	}
	if a.tenant.IsValid() && a.tenant != tenant {
		return nil, meshid.Tenant{}, refuseIdentity("private device alias %q was saved for organization %s; select that organization or choose a device explicitly", a.host, a.tenant)
	}
	return auth, tenant, nil
}

var privateAliasMeshRead = func(ctx context.Context) (sessionbroker.OperatorDirectory, error) { return meshDirectoryRead(ctx) }
var privateAliasLANCollect = discovery.CollectLANUntilTimeout
var privateAliasLANBrowse = func(ctx context.Context) ([]models.LANDevice, error) {
	// Keep the entire browse window: the normal picker collector can settle on
	// the local agent before a delayed peer or conflicting name responds. The
	// collector also returns confirmed sightings on a budget-expired shutdown.
	var backendErr error
	devices, err := privateAliasLANCollect(ctx, discovery.StreamOptions{
		OnBackendError: func(err error) { backendErr = err },
	}, privateAliasDiscoveryTimeout)
	if backendErr != nil {
		return devices, backendErr
	}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return nil, err
	}
	return devices, nil
}
var privateAliasCloudFetch = fetchCloudAssetsV2
var privateAliasPrepareAuth = prepareDefaultCloudCertificate

// Assignment in init avoids a dependency cycle through the selector front door.
var privateAliasConnectMesh func(context.Context, string, bool) (*SelectedDevice, bool, error)

func init() { privateAliasConnectMesh = connectNamedDeviceSelector }

var privateAliasEnforcePin = enforceDevicePinAt

type privateAliasSources struct {
	mesh     sessionbroker.OperatorDirectory
	lan      []models.LANDevice
	cloud    []*cloudpbv2.Asset
	meshErr  error
	lanErr   error
	cloudErr error
}

func (s privateAliasSources) failure(err error) error {
	for _, item := range []struct {
		source string
		err    error
	}{{"Wendy Net", s.meshErr}, {"LAN discovery", s.lanErr}, {"Cloud inventory", s.cloudErr}} {
		if item.err != nil {
			err = errors.Join(err, fmt.Errorf("%s: %w", item.source, item.err))
		}
	}
	return err
}

func gatherPrivateAliasSources(ctx context.Context, alias privateDeviceAlias, tenant meshid.Tenant, expected *certs.WendyIdentity, auth *config.AuthConfig) (privateAliasSources, *SelectedDevice, error) {
	ctx, cancel := context.WithCancel(ctx)
	var work sync.WaitGroup
	defer func() {
		cancel()
		work.Wait()
	}()
	var sources privateAliasSources
	type meshResult struct {
		directory sessionbroker.OperatorDirectory
		err       error
	}
	type lanResult struct {
		rows []models.LANDevice
		err  error
	}
	type cloudResult struct {
		assets []*cloudpbv2.Asset
		err    error
	}
	mesh := make(chan meshResult, 1)
	lan := make(chan lanResult, 1)
	cloud := make(chan cloudResult, 1)
	work.Add(1)
	go func(out chan<- meshResult) {
		defer work.Done()
		bounded, stop := context.WithTimeout(ctx, privateAliasMeshDiscoveryTimeout)
		defer stop()
		result, err := privateAliasMeshRead(bounded)
		out <- meshResult{result, err}
	}(mesh)
	startLAN := func(out chan<- lanResult, budget time.Duration) {
		work.Go(func() {
			bounded, stop := context.WithTimeout(ctx, budget)
			defer stop()
			rows, err := privateAliasLANBrowse(bounded)
			out <- lanResult{rows, err}
		})
	}
	lanBudget := privateAliasDiscoveryTimeout
	if alias.uuid != "" {
		lanBudget = privateAliasLANHeadStart
	}
	startLAN(lan, lanBudget)
	cloudAuth := *auth
	cloudAuth.Certificates = slices.Clone(auth.Certificates)
	work.Add(1)
	go func(out chan<- cloudResult) {
		defer work.Done()
		bounded, stop := context.WithTimeout(ctx, privateAliasDiscoveryTimeout)
		defer stop()
		if err := privateAliasPrepareAuth(bounded, &cloudAuth); err != nil {
			out <- cloudResult{err: err}
			return
		}
		if len(cloudAuth.Certificates) == 0 || cloudAuth.Certificates[0].TenantUUID() != tenant.String() {
			out <- cloudResult{err: errors.New("selected organization changed during private alias discovery")}
			return
		}
		assets, err := privateAliasCloudFetch(bounded, &cloudAuth, false)
		out <- cloudResult{assets, err}
	}(cloud)

	lanHeadStart := time.NewTimer(privateAliasLANHeadStart)
	defer lanHeadStart.Stop()
	grace := lanHeadStart.C
	nameWindow := time.NewTimer(privateAliasDiscoveryTimeout)
	defer nameWindow.Stop()
	nameDeadline := nameWindow.C
	extendedLAN := false
	var lanFailure error
	for {
		select {
		case <-ctx.Done():
			return sources, nil, ctx.Err()
		case <-grace:
			grace = nil
		case <-nameDeadline:
			nameDeadline = nil
		case result := <-mesh:
			mesh = nil
			sources.mesh, sources.meshErr = result.directory, result.err
		case result := <-lan:
			lan = nil
			sources.lan, sources.lanErr = result.rows, result.err
		case result := <-cloud:
			cloud = nil
			sources.cloud, sources.cloudErr = result.assets, result.err
			if len(cloudAuth.Certificates) != 0 && cloudAuth.Certificates[0].TenantUUID() == tenant.String() {
				*auth = cloudAuth
			}
		}
		if err := ctx.Err(); err != nil {
			return sources, nil, err
		}
		candidate, selectionErr := selectPrivateAliasCandidate(alias, tenant, expected, auth, sources, time.Now())
		if alias.uuid != "" && selectionErr == nil && len(candidate.lan) != 0 {
			selected, err := connectPrivateAliasLAN(ctx, alias, auth, candidate)
			if !privateAliasLANUnreachable(err) {
				return sources, selected, err
			}
			lanFailure, sources.lan = err, nil
		}
		// Inventory includes offline assets. Only a Cloud presence hint can
		// end discovery early while a cold Wendy Net join is still pending.
		cloudReady := candidate.cloud.GetLastHeartbeat() != nil
		// Process the initial browse even when its result and the grace
		// timer become ready together.
		if alias.uuid != "" && grace == nil && (lan == nil || extendedLAN) && selectionErr == nil && (candidate.mesh != "" || cloudReady) {
			return sources, nil, nil
		}
		if alias.uuid == "" && lan == nil && cloud == nil &&
			(mesh == nil || nameDeadline == nil && selectionErr == nil && (len(candidate.lan) != 0 || cloudReady)) {
			return sources, nil, nil
		}
		if lan == nil && alias.uuid != "" && !extendedLAN && lanFailure == nil && sources.lanErr == nil {
			extendedLAN = true
			lan = make(chan lanResult, 1)
			startLAN(lan, privateAliasDiscoveryTimeout-privateAliasLANHeadStart)
		}
		if mesh == nil && lan == nil && cloud == nil {
			return sources, nil, lanFailure
		}
	}
}

type privateAliasCandidate struct {
	device  meshid.Device
	trusted bool
	mesh    string
	lan     []models.LANDevice
	cloud   *cloudpbv2.Asset
}

// LAN names are hints, never identity authority. A friendly alias needs a saved
// binding, a signed mesh name, or authenticated Cloud inventory. Explicit UUID
// aliases already supply the expected device within the selected tenant.
func selectPrivateAliasCandidate(alias privateDeviceAlias, tenant meshid.Tenant, expected *certs.WendyIdentity, auth *config.AuthConfig, sources privateAliasSources, now time.Time) (privateAliasCandidate, error) {
	candidates := make(map[meshid.Device]*privateAliasCandidate)
	expectedUUID := alias.uuid
	if expected != nil {
		expectedUUID = expected.EntityID
	}
	matches := func(name string, device meshid.Device) bool {
		if expectedUUID != "" && device.UUID().String() == expectedUUID {
			return true
		}
		if alias.uuid != "" {
			return false
		}
		label, valid := meshname.ShortDeviceLabel(name)
		return valid && label == alias.label
	}
	add := func(device meshid.Device, trusted bool) *privateAliasCandidate {
		candidate := candidates[device]
		if candidate == nil {
			candidate = &privateAliasCandidate{device: device}
			candidates[device] = candidate
		}
		candidate.trusted = candidate.trusted || trusted
		return candidate
	}
	if expectedUUID != "" {
		device, err := meshid.ParseDevice(tenant, expectedUUID)
		if err != nil {
			return privateAliasCandidate{}, err
		}
		add(device, true)
	}
	snapshot := sources.mesh
	if snapshot.State == "ready" && snapshot.Captured <= now.Add(time.Second).UnixMilli() && now.UnixMilli()-snapshot.Captured <= 10000 {
		// A bounded directory may omit another device with the same name. Its
		// visible rows still route an explicit UUID, but cannot exclude friendly
		// name collisions, even when Cloud or a saved binding agrees with a row.
		if snapshot.Truncated && alias.uuid == "" {
			return privateAliasCandidate{}, fmt.Errorf("private device alias %q cannot be resolved safely from an incomplete mesh directory; use device-<uuid>.wendy.internal or a canonical mesh/Cloud selector", alias.host)
		}
		for _, row := range snapshot.Devices {
			device, err := meshid.DeviceFromPrincipal(row.Principal)
			if err != nil || device.Tenant() != tenant || row.Device != device.UUID().String() || row.Tenant != tenant.String() || row.Expires <= now.UnixMilli() || row.Selector != sessionbroker.MeshSelector(device) || !matches(row.Name, device) {
				continue
			}
			add(device, true).mesh = row.Selector
		}
	}
	for _, asset := range sources.cloud {
		if asset == nil || asset.GetOrganizationId() != tenant.String() {
			continue
		}
		if _, err := meshid.ParseDeviceUUID(asset.GetId()); err != nil {
			continue
		}
		identity := cloudDiscoveryIdentity(auth, cloudDiscoveryDevice{v2: asset, key: asset.GetId()})
		device, err := meshid.DeviceFromPrincipal(identity.Principal)
		if err != nil || device.Tenant() != tenant || !matches(asset.GetName(), device) {
			continue
		}
		candidate := add(device, true)
		if candidate.cloud != nil && candidate.cloud.GetId() != asset.GetId() {
			return privateAliasCandidate{}, fmt.Errorf("private device alias %q has conflicting Cloud asset bindings", alias.host)
		}
		candidate.cloud = asset
	}
	for _, row := range sources.lan {
		device, err := meshid.DeviceFromPrincipal(row.PrincipalURI)
		if err != nil || device.Tenant() != tenant {
			continue
		}
		candidate := candidates[device]
		if candidate == nil {
			if !matches(row.CloudName, device) {
				continue
			}
			candidate = add(device, alias.uuid != "")
		}
		candidate.lan = append(candidate.lan, row)
	}
	if len(candidates) > 1 {
		if expected != nil {
			for _, candidate := range candidates {
				if candidate.trusted && candidate.device.UUID().String() != expectedUUID {
					return privateAliasCandidate{}, refuseIdentity("private device alias %q now points to a different device; verify the device UUID and use 'wendy device unpin %s' before reconnecting", alias.host, alias.host)
				}
			}
		}
		return privateAliasCandidate{}, fmt.Errorf("private device alias %q is ambiguous; use its device UUID or canonical mesh/Cloud selector", alias.host)
	}
	for _, candidate := range candidates {
		if !candidate.trusted {
			return privateAliasCandidate{}, fmt.Errorf("cannot authenticate friendly private alias %q from LAN discovery alone: %w; try the same hostname when Cloud inventory is reachable, or use device-<uuid>.wendy.internal", alias.host, certs.ErrExpectedDeviceIdentity)
		}
		return *candidate, nil
	}
	return privateAliasCandidate{}, fmt.Errorf("private device alias %q was not found in the selected organization", alias.host)
}

func connectPrivateDeviceAlias(ctx context.Context, value string, suppress bool) (*SelectedDevice, bool, error) {
	alias, matched, err := parsePrivateDeviceAlias(value)
	if !matched || err != nil {
		return nil, matched, err
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, true, err
	}
	auth, tenant, err := alias.authScope(cfg)
	if err != nil {
		return nil, true, err
	}
	pinKey := alias.pinKey(tenant)
	expected, err := alias.expectedIdentity(cfg, tenant)
	if err != nil {
		return nil, true, err
	}
	if alias.uuid != "" && expected != nil && expected.EntityID != alias.uuid {
		return nil, true, refuseIdentity("private device alias %q does not match its saved device binding", alias.host)
	}
	sources, selected, err := gatherPrivateAliasSources(ctx, alias, tenant, expected, auth)
	if selected != nil {
		return selected, true, nil
	}
	if err != nil {
		return nil, true, sources.failure(err)
	}
	candidate, err := selectPrivateAliasCandidate(alias, tenant, expected, auth, sources, time.Now())
	if err != nil {
		return nil, true, sources.failure(err)
	}
	if len(candidate.lan) > 0 {
		selected, err := connectPrivateAliasLAN(ctx, alias, auth, candidate)
		if err == nil || !privateAliasLANUnreachable(err) || (candidate.mesh == "" && candidate.cloud == nil) {
			return selected, true, err
		}
	}
	if candidate.mesh != "" {
		selected, _, err := privateAliasConnectMesh(ctx, candidate.mesh, suppress)
		if err == nil && selected.Agent != nil {
			err = probePrivateAliasAgent(ctx, selected.Agent, privateAliasMeshProbeTimeout)
			if err != nil {
				selected.Close()
			}
		}
		if err == nil {
			if selected.Agent != nil {
				if err := privateAliasEnforcePin(pinKey, selected.Agent.Addr, selected.Agent); err != nil {
					selected.Close()
					return nil, true, err
				}
				selected.PinKey, selected.Agent.DevicePinKey = pinKey, pinKey
			}
			return selected, true, nil
		}
		// Discovery can precede a local router loss. Only the broker's typed
		// readiness failures permit another path to the same selected identity;
		// authentication, policy and unclassified transport failures stay terminal.
		recoverable, _ := sessionbroker.OperatorRecovery(err)
		if ctx.Err() != nil {
			return nil, true, ctx.Err()
		}
		if !recoverable || candidate.cloud == nil {
			return nil, true, err
		}
	}
	if candidate.cloud != nil {
		if err := privateAliasPrepareAuth(ctx, auth); err != nil {
			return nil, true, err
		}
		if len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() != tenant.String() {
			return nil, true, fmt.Errorf("selected organization changed during private alias connection")
		}
		selector := cloudDeviceSelector{Endpoint: auth.CloudGRPC, TenantUUID: tenant.String(), AssetUUID: candidate.cloud.GetId()}.String()
		if isLiteCloudAsset(candidate.cloud) {
			selected, err := cloudLiteSelectedDevice(auth, candidate.cloud, selector)
			return selected, true, err
		}
		conn, err := connectDefaultCloudAssetV2Fn(ctx, auth, candidate.cloud, os.Getenv("WENDY_BROKER_URL"))
		if err != nil {
			return nil, true, privateAliasCloudConnectError(ctx, auth, selector, candidate.cloud, err)
		}
		if err := probePrivateAliasAgent(ctx, conn, cloudAgentConnectTimeout); err != nil {
			conn.Close()
			return nil, true, privateAliasCloudConnectError(ctx, auth, selector, candidate.cloud, fmt.Errorf("authenticating the device through Cloud: %w", err))
		}
		if err := privateAliasEnforcePin(pinKey, conn.Addr, conn); err != nil {
			conn.Close()
			return nil, true, err
		}
		conn.DevicePinKey = pinKey
		return &SelectedDevice{Agent: conn, Selector: selector, PinKey: pinKey}, true, nil
	}
	return nil, true, sources.failure(fmt.Errorf("private device alias %q identifies %s but no reachable LAN, mesh or Cloud endpoint was found", alias.host, candidate.device.Principal()))
}

// Cloud inventory already confirmed enrollment. Ask for the online view
// after a transport-level refusal so an authentication failure keeps its
// own status, and an available device keeps its original transport error.
func privateAliasCloudConnectError(ctx context.Context, auth *config.AuthConfig, selector string, asset *cloudpbv2.Asset, err error) error {
	if status.Code(err) != codes.Unavailable {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, privateAliasDiscoveryTimeout)
	defer cancel()
	online, lookupErr := privateAliasCloudFetch(bounded, auth, true)
	if lookupErr != nil || slices.ContainsFunc(online, func(a *cloudpbv2.Asset) bool { return a.GetId() == asset.GetId() }) {
		return err
	}
	target, _, parseErr := parseCloudDeviceSelector(selector)
	if parseErr != nil {
		return err
	}
	return cloudSelectorMissError(auth, asset.GetId(), "tenant", config.OrganizationLabel(target.Endpoint, target.TenantUUID), nil,
		[]cloudDiscoveryDevice{{cloudAssetMetadata: asset, v2: asset, key: asset.GetId()}})
}

// Cloud creates a lazy TLS client, while a mesh proxy verifies the selected
// peer when it first forwards an RPC. Probe before pinning either connection.
func probePrivateAliasAgent(ctx context.Context, conn *grpcclient.AgentConnection, timeout time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, cached := conn.CachedAgentVersion(); cached {
		return nil
	}
	probe, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	version, err := conn.AgentService.GetAgentVersion(probe, &agentpb.GetAgentVersionRequest{})
	if err == nil {
		conn.CacheAgentVersion(version)
	}
	return err
}

func connectPrivateAliasLAN(ctx context.Context, alias privateDeviceAlias, auth *config.AuthConfig, candidate privateAliasCandidate) (*SelectedDevice, error) {
	var addresses []string
	seen := map[string]bool{}
	for _, row := range candidate.lan {
		for _, addr := range lanDialCandidates(ctx, row) {
			if !seen[addr] && len(addresses) < maxDialCandidates {
				seen[addr] = true
				addresses = append(addresses, addr)
			}
		}
	}
	if len(addresses) == 0 {
		return nil, &noAuthenticatedEndpointError{msg: "private alias has no LAN agent address"}
	}
	pinKey := alias.pinKey(candidate.device.Tenant())
	dial := func(ctx context.Context) (*grpcclient.AgentConnection, error) {
		credentials := *auth
		credentials.Certificates = slices.Clone(auth.Certificates)
		if err := privateAliasPrepareAuth(ctx, &credentials); err != nil {
			return nil, err
		}
		if len(credentials.Certificates) == 0 || credentials.Certificates[0].TenantUUID() != candidate.device.Tenant().String() {
			return nil, fmt.Errorf("selected organization changed during private alias connection")
		}
		target := newDialTargetCandidates(pinKey, addresses)
		identity := certs.WendyIdentity{TenantUUID: candidate.device.Tenant().String(), EntityType: certs.EntityAsset, EntityID: candidate.device.UUID().String(), Principal: candidate.device.Principal()}
		if target.Expected != nil && !target.Expected.SameEntity(identity) {
			return nil, refuseIdentity("private device alias %q changed identity", alias.host)
		}
		target.Expected = &identity
		conn, _, err := dialAgentLadderWithCertsFn(ctx, target, credentials.Certificates)
		if err != nil {
			return nil, err
		}
		if conn == nil || !conn.IsMTLS {
			if conn != nil {
				conn.Close()
			}
			return nil, certs.ErrExpectedDeviceIdentity
		}
		if err := privateAliasEnforcePin(pinKey, addresses[0], conn); err != nil {
			conn.Close()
			return nil, err
		}
		conn.Host, conn.DevicePinKey = alias.host, pinKey
		return conn, nil
	}
	conn, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	conn.Reconnect = dial
	return &SelectedDevice{Agent: conn, Selector: pinKey, PinKey: pinKey}, nil
}

// A transport that answered and refused authentication must never be hidden by
// another route. The ladder distinguishes silent/unreachable endpoints from
// certificate rejection and plaintext setup endpoints.
func privateAliasLANUnreachable(err error) bool {
	var failure *noAuthenticatedEndpointError
	return errors.As(err, &failure) && !failure.certSeen && failure.diagnostic == nil
}
