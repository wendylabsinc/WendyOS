package commands

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// DesktopCloudRouteIdentity is metadata obtained from authenticated Cloud
// inventory. Principal names the enrolled device, which older rows may bind to
// a different UUID than their asset ID. It is never inferred from the asset ID
// when inventory supplies an explicit PKI binding.
type DesktopCloudRouteIdentity struct{ Name, Principal string }

// DesktopCloudRouteIdentities refreshes bindings for selected Cloud routes. The
// native app passes selectors only; saved or caller-provided route principals
// must not establish a new binding. Legacy routes retain their existing names.
func DesktopCloudRouteIdentities(ctx context.Context, selectors []string) (map[string]DesktopCloudRouteIdentity, error) {
	needsCloud := false
	for _, text := range selectors {
		s, matched, err := parseCloudDeviceSelector(text)
		if err != nil {
			return nil, err
		}
		if matched && s.TenantUUID != "" {
			needsCloud = true
		}
	}
	if !needsCloud {
		return map[string]DesktopCloudRouteIdentity{}, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return desktopCloudRouteIdentities(ctx, cfg, selectors, func(ctx context.Context, auth *config.AuthConfig) ([]*cloudpbv2.Asset, error) {
		if err := prepareDefaultCloudCertificate(ctx, auth); err != nil {
			return nil, err
		}
		return fetchDefaultCloudAssetsV2Fn(ctx, auth, false)
	})
}

func desktopCloudRouteIdentities(ctx context.Context, cfg *config.Config, selectors []string, fetch func(context.Context, *config.AuthConfig) ([]*cloudpbv2.Asset, error)) (map[string]DesktopCloudRouteIdentity, error) {
	result := make(map[string]DesktopCloudRouteIdentity)
	cache := make(map[string][]*cloudpbv2.Asset)
	for _, text := range selectors {
		selector, matched, err := parseCloudDeviceSelector(text)
		if err != nil {
			return nil, err
		}
		if !matched || selector.TenantUUID == "" {
			continue
		}
		auth, err := selector.auth(cfg)
		if err != nil {
			return nil, err
		}
		key := selector.Endpoint + "/" + selector.TenantUUID
		assets, ok := cache[key]
		if !ok {
			assets, err = fetch(ctx, auth)
			if err != nil {
				return nil, err
			}
			cache[key] = assets
		}
		found := false
		for _, asset := range assets {
			if asset.GetId() != selector.AssetUUID || asset.GetOrganizationId() != selector.TenantUUID {
				continue
			}
			if found {
				return nil, fmt.Errorf("duplicate Cloud asset identity for route %s", text)
			}
			found = true
			expected := cloudDiscoveryIdentity(auth, cloudDiscoveryDevice{v2: asset, key: asset.GetId()})
			info := DesktopCloudRouteIdentity{Name: asset.GetName()}
			if peer, err := meshid.DeviceFromPrincipal(expected.Principal); err == nil {
				info.Principal = peer.Principal()
			}
			result[text] = info
		}
		if !found {
			return nil, fmt.Errorf("Cloud route asset is absent from its authenticated organization: %s", text)
		}
	}
	return result, nil
}
