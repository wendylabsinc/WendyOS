package clouddefaults

import (
	"strconv"
	"strings"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// FindAssetByNameOrID returns the first asset whose name equals needle
// (case-insensitive) or whose ID equals needle parsed as an integer; nil if
// none match. Name matches take priority over the numeric-ID fallback,
// mirroring resolveCloudAsset's matching in commands/cloud_tunnel.go — but
// unlike resolveCloudAsset this returns the first hit rather than erroring
// on multiple name matches, since callers here (offline re-checks, the mcp
// package which cannot import commands) just need a single best-effort
// lookup, not the CLI's ambiguity-reporting UX.
//
// Callers must pass a COMPUTE-DEVICE-ONLY list. Cloud scopes device-name
// uniqueness to compute devices (WDY-3016), so a building or vehicle asset may
// legally share a device's name and would shadow it in this first-match scan.
// Both callers already ask cloud for that filter --
// commands.fetchCloudAssetsFiltered and mcp.mcpListCloudAssets each set
// ListAssetsRequest.IsComputeDevice, so there is no client-side filter here to
// go stale; a future caller that lists assets unfiltered has to filter first.
func FindAssetByNameOrID(assets []*cloudpb.Asset, needle string) *cloudpb.Asset {
	if needle == "" {
		return nil
	}
	lower := strings.ToLower(needle)
	for _, a := range assets {
		if strings.ToLower(a.GetName()) == lower {
			return a
		}
	}
	if id, err := strconv.Atoi(strings.TrimSpace(needle)); err == nil {
		for _, a := range assets {
			if a.GetId() == int32(id) {
				return a
			}
		}
	}
	return nil
}
