package commands

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// fetchDefaultCloudAllAssetsFn lists a legacy organization's devices, offline
// ones included. It only explains a miss, so unlike fetchDefaultCloudAssetsFn
// it does not seed pins.
var fetchDefaultCloudAllAssetsFn = func(ctx context.Context, auth *config.AuthConfig) ([]*cloudpb.Asset, error) {
	return fetchCloudAssetsFiltered(ctx, auth, false)
}

// cloudSelectorChooseStep is the hint a selector miss has always ended with.
const cloudSelectorChooseStep = "Choose another device with --device or 'wendy device set-default'."

// explainCloudSelectorMissV2 explains why a tenant selector's asset is not
// among the online devices. It asks Cloud once more, for every enrolled
// device, and returns nil when that fails, so the caller keeps its own message.
func explainCloudSelectorMissV2(ctx context.Context, auth *config.AuthConfig, selector cloudDeviceSelector, online []*cloudpbv2.Asset) error {
	all, err := fetchDefaultCloudAssetsV2Fn(ctx, auth, false)
	if err != nil {
		return nil
	}
	devices := func(assets []*cloudpbv2.Asset) []cloudDiscoveryDevice {
		out := make([]cloudDiscoveryDevice, 0, len(assets))
		for _, a := range assets {
			out = append(out, cloudDiscoveryDevice{cloudAssetMetadata: a, v2: a, key: a.GetId()})
		}
		return out
	}
	return cloudSelectorMissError(auth, selector.AssetUUID, "tenant", config.OrganizationLabel(selector.Endpoint, selector.TenantUUID), devices(online), devices(all))
}

// explainCloudSelectorMissLegacy is explainCloudSelectorMissV2 for a numeric
// organization selector.
func explainCloudSelectorMissLegacy(ctx context.Context, auth *config.AuthConfig, selector cloudDeviceSelector, online []*cloudpb.Asset) error {
	all, err := fetchDefaultCloudAllAssetsFn(ctx, auth)
	if err != nil {
		return nil
	}
	return cloudSelectorMissError(auth, fmt.Sprint(selector.AssetID), "organization", orgLabel(selector.Endpoint, selector.OrgID), legacyDiscoveryDevices(online), legacyDiscoveryDevices(all))
}

// cloudSelectorMissError tells an asset Cloud reports offline from one that is
// not enrolled any more: unenrolled, or enrolled again under a new ID. id is
// the selector's asset, scope and label its tenant or organization, online
// the devices Cloud reports online and all every enrolled device.
func cloudSelectorMissError(auth *config.AuthConfig, id, scope, label string, online, all []cloudDiscoveryDevice) error {
	i := slices.IndexFunc(all, func(d cloudDiscoveryDevice) bool { return d.key == id })
	if i < 0 {
		return withNextSteps(commandErrorf(errNoDevice, "cloud asset %s is not enrolled in %s %s; it may have been unenrolled, or enrolled again under a new ID", id, scope, label),
			"Find the device's current entry with 'wendy cloud discover --all --json'.", cloudSelectorChooseStep)
	}
	// Enrolling a device again may leave its old record behind, which then
	// stays offline for good, so an offline asset also points at a new entry.
	reenrolled := "If the device was enrolled again, it has a new entry: find it with 'wendy cloud discover --all --json'."
	if entry, ok := cloudSelectorNewEntry(auth, all[i], online, all); ok {
		reenrolled = fmt.Sprintf("Another device named %q is enrolled in this %s. If this one was enrolled again, use the new entry: %s", all[i].GetName(), scope, entry)
	}
	return withNextSteps(commandErrorf(errDeviceOffline, "cloud asset %s in %s %s is enrolled but currently reported offline", id, scope, label),
		"Check the device's power and network connection, then retry.", reenrolled, cloudSelectorChooseStep)
}

// cloudSelectorNewEntry returns the selector of the one other device that
// carries old's name: what an enrollment that left the old record behind looks
// like. A device that is online is preferred. When several qualify, none is
// singled out.
func cloudSelectorNewEntry(auth *config.AuthConfig, old cloudDiscoveryDevice, online, all []cloudDiscoveryDevice) (string, bool) {
	if old.GetName() == "" {
		return "", false
	}
	sameName := func(devices []cloudDiscoveryDevice) []cloudDiscoveryDevice {
		var out []cloudDiscoveryDevice
		for _, d := range devices {
			if d.key != old.key && strings.EqualFold(d.GetName(), old.GetName()) {
				out = append(out, d)
			}
		}
		return out
	}
	candidates := sameName(online)
	if len(candidates) == 0 {
		candidates = sameName(all)
	}
	if len(candidates) != 1 {
		return "", false
	}
	entry, err := cloudDiscoveryDeviceDefault(auth, candidates[0])
	return entry, err == nil
}
