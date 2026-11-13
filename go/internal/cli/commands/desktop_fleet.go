package commands

import (
	"context"
	"errors"
	"time"
)

type DesktopFleetDevice struct {
	ID, Name, Selector, SiteID, Site, Hardware, Address string
	Online                                              bool
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
	assets, err := fetchCloudAssetsV2(ctx, auth, false)
	if err != nil {
		return DesktopFleet{}, err
	}
	// Query broker presence, never infer online status from a stale timestamp.
	online, presenceErr := fetchCloudAssetsV2(ctx, auth, true)
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
		item := DesktopFleetDevice{ID: a.GetId(), Name: name, Selector: selector, SiteID: a.GetSiteId(), Site: a.GetSite(), Hardware: a.GetDeviceType(), Address: a.GetIpAddress(), Online: present[a.GetId()]}
		if stamp := a.GetLastHeartbeat(); stamp != nil && stamp.IsValid() && stamp.AsTime().Before(time.Now().Add(time.Minute)) {
			item.LastSeen = stamp.GetSeconds()
		}
		out.Devices = append(out.Devices, item)
	}
	return out, nil
}
