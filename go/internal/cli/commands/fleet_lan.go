package commands

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// cloudTargetsForTags returns one connectable cloud target per enrolled asset
// carrying any of the given tags (assigned via 'wendy fleet group add').
func cloudTargetsForTags(auth *config.AuthConfig, assets []*cloudpb.Asset, tags []string, brokerURL string) []fleetTarget {
	matched := assetsWithAnyTag(assets, tags)
	targets := make([]fleetTarget, 0, len(matched))
	for _, asset := range matched {
		asset := asset
		targets = append(targets, fleetTarget{
			Name: asset.GetName(),
			ID:   fmt.Sprintf("%d", asset.GetId()),
			connect: func(ctx context.Context) (*grpcclient.AgentConnection, error) {
				return connectCloudAsset(ctx, auth, asset, brokerURL)
			},
		})
	}
	return targets
}

// fleetLANDiscoverTimeout is the default mDNS browse window for a fleet
// operation when no explicit timeout is given.
const fleetLANDiscoverTimeout = 5 * time.Second

// fleetTarget is one device a fleet operation acts on, regardless of whether it
// was resolved over the LAN (mDNS) or the cloud. connect dials the device's
// agent on demand so a target can be enumerated cheaply and only connected when
// the operation actually touches it.
type fleetTarget struct {
	Name    string // display name (LAN: normalized short name; cloud: asset name)
	ID      string // stable id (LAN: mDNS hostname; cloud: asset id as string)
	Address string // LAN dial address (empty for cloud targets)
	connect func(ctx context.Context) (*grpcclient.AgentConnection, error)
}

// groupPatternChars allows the same characters as a cloud group name plus the
// glob metacharacters a dynamic LAN match may use.
var groupPatternChars = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._*?\[\]-]{0,62}$`)

// validateGroupPattern checks a LAN group pattern: a cloud-style group name
// optionally containing glob metacharacters (* ? [ ]). Empty is rejected here;
// callers treat an absent --group as "every device" before calling this.
func validateGroupPattern(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("group pattern is required")
	}
	if !groupPatternChars.MatchString(pattern) {
		return fmt.Errorf("group pattern %q is invalid: start with a letter or digit, then letters, digits, '.', '_', '-', or glob characters '*?[]' (max 63 chars)", pattern)
	}
	return nil
}

// deviceShortName normalizes a LAN device's mDNS hostname to the bare device
// name used for group matching: "wendyos-camera-01.local" -> "camera-01". Falls
// back to a slugged display name when the hostname is empty.
func deviceShortName(dev models.LANDevice) string {
	name := strings.ToLower(strings.TrimSpace(dev.Hostname))
	name = strings.TrimSuffix(name, ".")
	name = strings.TrimSuffix(name, ".local")
	name = strings.TrimPrefix(name, "wendyos-")
	if name == "" {
		name = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(dev.DisplayName), " ", "-"))
	}
	return name
}

// matchesGroupPattern reports whether a LAN device belongs to a dynamic group.
// A group is a glob (path.Match) over the device's normalized short name; a
// plain token (no glob metacharacters) matches exactly or as a "<token>-"
// prefix, so "camera" matches camera-01..camera-04. An empty pattern, "*", or
// "all" matches every WendyOS device.
func matchesGroupPattern(dev models.LANDevice, pattern string) bool {
	p := strings.ToLower(strings.TrimSpace(pattern))
	if p == "" || p == "*" || p == "all" {
		return true
	}
	name := deviceShortName(dev)
	if strings.ContainsAny(p, "*?[") {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
		ok, _ := path.Match(p, strings.ToLower(dev.Hostname))
		return ok
	}
	return name == p || strings.HasPrefix(name, p+"-")
}

// discoverFleetLAN returns the WendyOS devices visible on the LAN via mDNS,
// sorted by short name for stable output.
func discoverFleetLAN(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error) {
	if timeout <= 0 {
		timeout = fleetLANDiscoverTimeout
	}
	devices, err := discovery.CollectLAN(ctx, cliLANStreamOptions(ctx), timeout)
	if err != nil {
		return nil, fmt.Errorf("discovering LAN devices: %w", err)
	}
	out := make([]models.LANDevice, 0, len(devices))
	for _, d := range devices {
		if d.IsWendyDevice {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return deviceShortName(out[i]) < deviceShortName(out[j]) })
	return out, nil
}

// lanGroupDevices discovers LAN devices and filters them to the group pattern
// (all WendyOS devices when group is empty).
func lanGroupDevices(ctx context.Context, group string, timeout time.Duration) ([]models.LANDevice, error) {
	devices, err := discoverFleetLAN(ctx, timeout)
	if err != nil {
		return nil, err
	}
	if group == "" {
		return devices, nil
	}
	var out []models.LANDevice
	for _, dev := range devices {
		if matchesGroupPattern(dev, group) {
			out = append(out, dev)
		}
	}
	return out, nil
}

// lanDevicesForTags discovers LAN devices and returns those whose name matches
// any of the given tags (each a glob, e.g. "camera-*"). A device that matches
// more than one tag appears once.
func lanDevicesForTags(ctx context.Context, tags []string, timeout time.Duration) ([]models.LANDevice, error) {
	devices, err := discoverFleetLAN(ctx, timeout)
	if err != nil {
		return nil, err
	}
	var out []models.LANDevice
	for _, dev := range devices {
		for _, tag := range tags {
			if matchesGroupPattern(dev, tag) {
				out = append(out, dev)
				break
			}
		}
	}
	return out, nil
}

// targetForDevice wraps a discovered LAN device as a connectable fleet target.
func targetForDevice(dev models.LANDevice) fleetTarget {
	addr := preferredLANAddress(dev)
	return fleetTarget{
		Name:    deviceShortName(dev),
		ID:      dev.Hostname,
		Address: addr,
		connect: func(ctx context.Context) (*grpcclient.AgentConnection, error) {
			return connectWithAutoTLS(ctx, addr)
		},
	}
}

// lanFleetTargets discovers LAN devices, filters them to the group pattern (all
// WendyOS devices when group is empty), and returns one connectable target each.
func lanFleetTargets(ctx context.Context, group string, timeout time.Duration) ([]fleetTarget, error) {
	devices, err := lanGroupDevices(ctx, group, timeout)
	if err != nil {
		return nil, err
	}
	targets := make([]fleetTarget, 0, len(devices))
	for _, dev := range devices {
		targets = append(targets, targetForDevice(dev))
	}
	return targets, nil
}

// peerHost returns the address other components should use to reach dev's
// exposed endpoint. It prefers the routable IP (resolvable from any peer,
// including WendyOS devices that may not run an mDNS resolver) and falls back to
// the mDNS hostname.
func peerHost(dev models.LANDevice) string {
	if dev.IPAddress != "" {
		return dev.IPAddress
	}
	return strings.TrimSuffix(dev.Hostname, ".")
}

// cloudFleetTargets resolves a group's member devices from the cloud asset list
// (tag membership) and returns one connectable target each, dialing over the
// cloud tunnel.
func cloudFleetTargets(ctx context.Context, group, cloudGRPC, brokerURL string) ([]fleetTarget, error) {
	auth, err := pickAuthEntry(cloudGRPC)
	if err != nil {
		return nil, err
	}
	assets, err := fetchCloudAssetsFiltered(ctx, auth, false)
	if err != nil {
		return nil, err
	}
	seedPinsFromAssetsBestEffort(auth, assets)
	if group != "" {
		assets = assetsInGroup(assets, group)
	}
	targets := make([]fleetTarget, 0, len(assets))
	for _, asset := range assets {
		asset := asset
		targets = append(targets, fleetTarget{
			Name: asset.GetName(),
			ID:   fmt.Sprintf("%d", asset.GetId()),
			connect: func(ctx context.Context) (*grpcclient.AgentConnection, error) {
				return connectCloudAsset(ctx, auth, asset, brokerURL)
			},
		})
	}
	return targets, nil
}

// resolveFleetTargets returns the devices a fleet operation should act on,
// choosing the LAN (mDNS) or cloud (asset tags) backend. group may be empty to
// mean "every device" (LAN: all WendyOS devices; cloud: all enrolled assets).
func resolveFleetTargets(ctx context.Context, group string, lan bool, cloudGRPC, brokerURL string, timeout time.Duration) ([]fleetTarget, error) {
	if lan {
		if group != "" {
			if err := validateGroupPattern(group); err != nil {
				return nil, err
			}
		}
		return lanFleetTargets(ctx, group, timeout)
	}
	if group != "" {
		if err := validateGroupName(group); err != nil {
			return nil, err
		}
	}
	return cloudFleetTargets(ctx, group, cloudGRPC, brokerURL)
}
