package devices

import (
	"strings"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

// ExpectedIdentityForPin preserves the CLI's expected certificate identity:
// prefer a valid, complete SPIFFE principal, otherwise use the legacy org/asset
// constraint. An assetless legacy pin supplies no certificate identity.
func ExpectedIdentityForPin(pin DevicePin) *platform.DeviceIdentity {
	if pin.Principal != "" {
		if id, ok := identityForPrincipal(pin.Principal); ok {
			return &id
		}
	}
	if pin.AssetID == "" {
		return nil
	}
	return &platform.DeviceIdentity{OrgID: int32(pin.OrgID), EntityType: "asset", EntityID: pin.AssetID}
}

// This is the CLI's ParsePrincipal projection, without certificate parsing or
// verification. Keep the complete principal alongside the legacy entity fields:
// distinct principal kinds can have the same projected entity type and ID.
func identityForPrincipal(principal string) (platform.DeviceIdentity, bool) {
	rest, ok := strings.CutPrefix(principal, "spiffe://wendy.sh/tenant/")
	if !ok {
		return platform.DeviceIdentity{}, false
	}
	tenant, kindAndName, ok := strings.Cut(rest, "/")
	if !ok || len(tenant) != 36 {
		return platform.DeviceIdentity{}, false
	}
	// Match uuid.Parse(tenant).String() == tenant in the original CLI: only
	// canonical lowercase UUID spelling is accepted, irrespective of version.
	for i, c := range tenant {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return platform.DeviceIdentity{}, false
			}
		} else if !strings.ContainsRune("0123456789abcdef", c) {
			return platform.DeviceIdentity{}, false
		}
	}
	kind, name, ok := strings.Cut(kindAndName, "/")
	if !ok || name == "" {
		return platform.DeviceIdentity{}, false
	}
	id := platform.DeviceIdentity{TenantUUID: tenant, Principal: principal, EntityID: name}
	switch kind {
	case "operator":
		id.EntityType = "user"
	case "device":
		id.EntityType = "asset"
	case "signer":
		id.EntityType = "signer"
	case "service":
		switch {
		case strings.HasPrefix(name, "asset-"):
			id.EntityType, id.EntityID = "asset", strings.TrimPrefix(name, "asset-")
		case strings.HasPrefix(name, "user-"):
			id.EntityType, id.EntityID = "user", strings.TrimPrefix(name, "user-")
		default:
			id.EntityType = "user"
		}
		if id.EntityID == "" {
			return platform.DeviceIdentity{}, false
		}
	default:
		return platform.DeviceIdentity{}, false
	}
	return id, true
}
