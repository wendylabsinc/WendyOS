package commands

import (
	"context"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
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

// DesktopFleetReader exposes the existing authenticated reads to app backends.
// It selects one saved identity without changing the active CLI context.
type DesktopFleetReader struct{ auth *config.AuthConfig }

func NewDesktopFleetReader(account, org string) (*DesktopFleetReader, error) {
	if org == "" {
		return nil, errors.New("choose an organization")
	}
	auth, err := desktopCloudAuth(account, org)
	if err != nil {
		return nil, err
	}
	return &DesktopFleetReader{auth: auth}, nil
}

func (r *DesktopFleetReader) Assets(ctx context.Context, onlineOnly bool) ([]*cloudpbv2.Asset, error) {
	return fetchCloudAssetsV2(ctx, r.auth, onlineOnly)
}

// DesktopFleetOverview retains its sequential CLI lookup behavior.
func DesktopFleetOverview(ctx context.Context, account, org string) (DesktopFleet, error) {
	reader, err := NewDesktopFleetReader(account, org)
	if err != nil {
		return DesktopFleet{}, err
	}
	assets, err := reader.Assets(ctx, false)
	if err != nil {
		return DesktopFleet{}, err
	}
	online, presenceErr := reader.Assets(ctx, true)
	return reader.Overview(assets, online, presenceErr)
}

// Overview preserves selector validation and the existing partial-presence result.
func (r *DesktopFleetReader) Overview(assets, online []*cloudpbv2.Asset, presenceErr error) (DesktopFleet, error) {
	auth := r.auth
	present := make(map[string]bool, len(online))
	for _, a := range online {
		present[a.GetId()] = true
	}
	out := DesktopFleet{PresenceError: cloudError(presenceErr)}
	for _, a := range assets {
		selector, err := cloudDiscoverySelector(auth, cloudDiscoveryDevice{cloudAssetMetadata: a, v2: a, key: a.GetId()})
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
