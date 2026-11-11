package commands

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshname"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

const privateAliasDiscoveryTimeout = 3 * time.Second

type privateDeviceAlias struct {
	host  string
	label string
	uuid  string
}

// Private device aliases select an enrolled identity, rather than asking the
// operating system to resolve the private suffix. In particular, no desktop VPN
// is necessary for CLI selection. The previous private namespace keeps its
// existing resolution behavior.
func parsePrivateDeviceAlias(value string) (privateDeviceAlias, bool, error) {
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
	if strings.HasSuffix(host, "."+meshname.PreviousSuffix) {
		return privateDeviceAlias{}, false, nil
	}
	if !strings.HasSuffix(host, "."+meshname.Suffix) && host != meshname.Suffix {
		return privateDeviceAlias{}, false, nil
	}
	alias := privateDeviceAlias{host: host, label: strings.TrimSuffix(host, "."+meshname.Suffix)}
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
	if !ok || host == meshname.Suffix {
		return alias, true, fmt.Errorf("invalid private device alias %q", value)
	}
	alias.label = label
	return alias, true, nil
}

var privateAliasMeshRead = func(ctx context.Context) (sessionbroker.OperatorDirectory, error) { return meshDirectoryRead(ctx) }
var privateAliasLANCollect = discovery.CollectLANUntilTimeout
var privateAliasLANBrowse = func(ctx context.Context) ([]models.LANDevice, error) {
	// Keep the entire browse window: the normal picker collector can settle on
	// the local agent before a delayed peer or conflicting name responds. The
	// collector also returns confirmed sightings on a budget-expired shutdown.
	devices, err := privateAliasLANCollect(ctx, discovery.StreamOptions{}, privateAliasDiscoveryTimeout)
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
	cloudErr error
}

func gatherPrivateAliasSources(ctx context.Context, auth *config.AuthConfig) privateAliasSources {
	var sources privateAliasSources
	selectedTenant := auth.Certificates[0].TenantUUID()
	var work sync.WaitGroup
	work.Add(3)
	bounded, cancel := context.WithTimeout(ctx, privateAliasDiscoveryTimeout)
	defer cancel()
	go func() { defer work.Done(); sources.mesh, _ = privateAliasMeshRead(bounded) }()
	go func() { defer work.Done(); sources.lan, _ = privateAliasLANBrowse(bounded) }()
	go func() {
		defer work.Done()
		// Inventory uses mTLS too. Renew the selected session before listing,
		// rather than waiting until an asset has already been found to dial.
		if err := privateAliasPrepareAuth(bounded, auth); err != nil {
			sources.cloudErr = err
			return
		}
		if len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() != selectedTenant {
			sources.cloudErr = errors.New("selected organization changed during private alias discovery")
			return
		}
		sources.cloud, sources.cloudErr = privateAliasCloudFetch(bounded, auth, false)
	}()
	work.Wait()
	return sources
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
		label, valid := meshname.FriendlyLabel(name)
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
	for _, row := range sources.lan {
		device, err := meshid.DeviceFromPrincipal(row.PrincipalURI)
		if err != nil || device.Tenant() != tenant || !matches(row.MeshName, device) {
			continue
		}
		candidate := add(device, alias.uuid != "")
		candidate.lan = append(candidate.lan, row)
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
	if len(candidates) > 1 {
		return privateAliasCandidate{}, fmt.Errorf("private device alias %q is ambiguous; use its device UUID or canonical mesh/Cloud selector", alias.host)
	}
	for _, candidate := range candidates {
		if !candidate.trusted {
			return privateAliasCandidate{}, fmt.Errorf("cannot authenticate friendly private alias %q from LAN discovery alone: %w; refresh trusted inventory with 'wendy cloud discover' or use device-<uuid>.wendy.internal", alias.host, certs.ErrExpectedDeviceIdentity)
		}
		return *candidate, nil
	}
	if sources.cloudErr != nil {
		return privateAliasCandidate{}, fmt.Errorf("private device alias %q was not found in the selected organization (Cloud inventory unavailable: %w)", alias.host, sources.cloudErr)
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
	auth, err := config.ResolveAuth(cfg, "", nil)
	if err != nil {
		return nil, true, err
	}
	if len(auth.Certificates) == 0 {
		return nil, true, certs.ErrExpectedDeviceIdentity
	}
	tenant, err := meshid.ParseTenant(auth.Certificates[0].TenantUUID())
	if err != nil {
		return nil, true, fmt.Errorf("private device aliases require a selected enrolled organization: %w", err)
	}
	// Only deterministic selected-name keys are consulted, never discovery aliases.
	var expected *certs.WendyIdentity
	for _, key := range []string{alias.host, alias.label} {
		if pin, ok := cfg.DevicePinFor(key); ok {
			identity := expectedIdentityForPin(pin)
			if identity == nil || identity.EntityType != certs.EntityAsset || identity.TenantUUID != tenant.String() {
				return nil, true, refuseIdentity("private device alias %q has a saved binding outside the selected organization", alias.host)
			}
			if expected != nil && !expected.SameEntity(*identity) {
				return nil, true, refuseIdentity("private device alias %q has conflicting saved bindings", alias.host)
			}
			expected = identity
		}
	}
	if alias.uuid != "" && expected != nil && expected.EntityID != alias.uuid {
		return nil, true, refuseIdentity("private device alias %q does not match its saved device binding", alias.host)
	}
	sources := gatherPrivateAliasSources(ctx, auth)
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	candidate, err := selectPrivateAliasCandidate(alias, tenant, expected, auth, sources, time.Now())
	if err != nil {
		return nil, true, err
	}
	if candidate.mesh != "" {
		selected, _, err := privateAliasConnectMesh(ctx, candidate.mesh, suppress)
		if err != nil {
			return nil, true, err
		}
		if selected.Agent != nil {
			if err := privateAliasEnforcePin(alias.host, selected.Agent.Addr, selected.Agent); err != nil {
				selected.Close()
				return nil, true, err
			}
		}
		return selected, true, nil
	}
	if len(candidate.lan) > 0 {
		selected, err := connectPrivateAliasLAN(ctx, alias, auth, candidate)
		if err == nil || candidate.cloud == nil || !privateAliasLANUnreachable(err) {
			return selected, true, err
		}
		// Only absence of an authenticated endpoint permits another independently
		// authenticated path. Identity/key/certificate refusals remain terminal.
	}
	if candidate.cloud != nil {
		if err := privateAliasPrepareAuth(ctx, auth); err != nil {
			return nil, true, err
		}
		if auth.Certificates[0].TenantUUID() != tenant.String() {
			return nil, true, fmt.Errorf("selected organization changed during private alias connection")
		}
		selector := cloudDeviceSelector{Endpoint: auth.CloudGRPC, TenantUUID: tenant.String(), AssetUUID: candidate.cloud.GetId()}.String()
		if isLiteCloudAsset(candidate.cloud) {
			selected, err := cloudLiteSelectedDevice(auth, candidate.cloud, selector)
			return selected, true, err
		}
		conn, err := connectDefaultCloudAssetV2Fn(ctx, auth, candidate.cloud, os.Getenv("WENDY_BROKER_URL"))
		if err != nil {
			return nil, true, err
		}
		if err := privateAliasEnforcePin(alias.host, conn.Addr, conn); err != nil {
			conn.Close()
			return nil, true, err
		}
		return &SelectedDevice{Agent: conn, DefaultSelector: selector, PinKey: alias.host}, true, nil
	}
	return nil, true, fmt.Errorf("private device alias %q identifies %s but no reachable LAN, mesh or Cloud endpoint was found", alias.host, candidate.device.Principal())
}

func connectPrivateAliasLAN(ctx context.Context, alias privateDeviceAlias, auth *config.AuthConfig, candidate privateAliasCandidate) (*SelectedDevice, error) {
	if err := privateAliasPrepareAuth(ctx, auth); err != nil {
		return nil, err
	}
	if len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() != candidate.device.Tenant().String() {
		return nil, fmt.Errorf("selected organization changed during private alias connection")
	}
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
		return nil, errors.New("private alias has no LAN agent address")
	}
	target := newDialTargetCandidates(alias.host, addresses)
	identity := certs.WendyIdentity{TenantUUID: candidate.device.Tenant().String(), EntityType: certs.EntityAsset, EntityID: candidate.device.UUID().String(), Principal: candidate.device.Principal()}
	if target.Expected != nil && !target.Expected.SameEntity(identity) {
		return nil, refuseIdentity("private device alias %q changed identity", alias.host)
	}
	target.Expected = &identity
	conn, _, err := dialAgentLadderWithCertsFn(ctx, target, auth.Certificates)
	if err != nil {
		return nil, err
	}
	if conn == nil || !conn.IsMTLS {
		if conn != nil {
			conn.Close()
		}
		return nil, certs.ErrExpectedDeviceIdentity
	}
	if err := privateAliasEnforcePin(alias.host, addresses[0], conn); err != nil {
		conn.Close()
		return nil, err
	}
	conn.Host, conn.DevicePinKey = alias.host, alias.host
	return &SelectedDevice{Agent: conn, DefaultSelector: alias.host, PinKey: alias.host}, nil
}

// A transport that answered and refused authentication must never be hidden by
// another route. The ladder distinguishes silent/unreachable endpoints from
// certificate rejection and plaintext setup endpoints.
func privateAliasLANUnreachable(err error) bool {
	var failure *noAuthenticatedEndpointError
	return errors.As(err, &failure) && !failure.certSeen && failure.diagnostic == nil
}
