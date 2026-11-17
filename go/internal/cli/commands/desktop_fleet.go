package commands

import (
	"context"
	"errors"
	"time"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

type DesktopFleetDevice struct {
	ID, Name, Selector, SiteID, Site, Hardware, Address string
	Online, Lite                                        bool
	LastSeen                                            int64
}
type DesktopFleet struct {
	Devices       []DesktopFleetDevice
	PresenceError string
}

func DesktopFleetOverview(ctx context.Context, account, org string) (DesktopFleet, error) {
	if org == "" {
		return DesktopFleet{}, errors.New("choose an organization")
	}
	auth, err := desktopCloudAuth(account, org)
	if err != nil {
		return DesktopFleet{}, err
	}
	assets, online, presenceErr, err := fetchFleetAssets(ctx, func(ctx context.Context, onlineOnly bool) ([]*cloudpbv2.Asset, error) {
		return fetchCloudAssetsV2(ctx, auth, onlineOnly)
	})
	if err != nil {
		return DesktopFleet{}, err
	}
	present := make(map[string]bool, len(online))
	for _, a := range online {
		present[a.GetId()] = true
	}
	out := DesktopFleet{PresenceError: cloudError(presenceErr)}
	for _, a := range assets {
		selector, err := cloudDiscoveryDeviceDefault(auth, cloudDiscoveryDevice{cloudAssetMetadata: a, v2: a, key: a.GetId()})
		if err != nil {
			return DesktopFleet{}, err
		}
		name := a.GetDisplayName()
		if name == "" {
			name = a.GetName()
		}
		item := DesktopFleetDevice{ID: a.GetId(), Name: name, Selector: selector, SiteID: a.GetSiteId(), Site: a.GetSite(), Hardware: a.GetDeviceType(), Address: a.GetIpAddress(), Online: present[a.GetId()], Lite: isLiteCloudAsset(a)}
		if stamp := a.GetLastHeartbeat(); stamp != nil && stamp.IsValid() && stamp.AsTime().Before(time.Now().Add(time.Minute)) {
			item.LastSeen = stamp.GetSeconds()
		}
		out.Devices = append(out.Devices, item)
	}
	return out, nil
}

// Fetch independent inventory and broker presence concurrently. Inventory errors
// fail the refresh; presence errors still allow the inventory to be displayed.
func fetchFleetAssets(ctx context.Context, fetch func(context.Context, bool) ([]*cloudpbv2.Asset, error)) (assets, online []*cloudpbv2.Asset, presenceErr, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		assets []*cloudpbv2.Asset
		err    error
	}
	presence := make(chan result, 1)
	go func() {
		items, e := fetch(ctx, true)
		presence <- result{items, e}
	}()
	assets, err = fetch(ctx, false)
	if err != nil {
		return nil, nil, nil, err
	}
	select {
	case r := <-presence:
		return assets, r.assets, r.err, nil
	case <-ctx.Done():
		return assets, nil, ctx.Err(), nil
	}
}
