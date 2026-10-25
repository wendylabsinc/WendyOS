package enrollment

import (
	"fmt"
	"strings"
)

const tenantSPIFFEPrefix = "spiffe://wendy.sh/tenant/"
const (
	kindOperator = "operator"
	kindDevice   = "device"
	kindService  = "service"
	kindSigner   = "signer"
	EntityUser   = "user"
	EntityAsset  = "asset"
	EntitySigner = "signer"
)

// Principal is a tenant-scoped public identity parsed from its SPIFFE URI.
type Principal struct{ TenantUUID, Principal, EntityID, EntityType string }

// ParsePrincipal preserves Wendy's operator, service, device and signer identities.
func ParsePrincipal(principal string) (Principal, error) {
	rest, ok := strings.CutPrefix(principal, tenantSPIFFEPrefix)
	if !ok {
		return Principal{}, fmt.Errorf("not a wendy tenant SPIFFE principal: %s", principal)
	}
	tenant, kindAndName, ok := strings.Cut(rest, "/")
	if !ok {
		return Principal{}, fmt.Errorf("SPIFFE principal has no kind: %s", principal)
	}
	// pki-core routes by the tenant UUID and compares it canonically, so a
	// non-canonical spelling of the same tenant is a different string to every
	// downstream comparison. Reject it here rather than let two spellings of one
	// tenant fail to match each other.
	if !canonicalUUID(tenant) {
		return Principal{}, fmt.Errorf("SPIFFE principal has non-canonical tenant UUID: %s", principal)
	}
	kind, name, ok := strings.Cut(kindAndName, "/")
	if !ok || name == "" {
		return Principal{}, fmt.Errorf("SPIFFE principal has no name: %s", principal)
	}

	id := Principal{TenantUUID: tenant, Principal: principal, EntityID: name}
	switch kind {
	case kindOperator:
		id.EntityType = EntityUser
	case kindDevice:
		id.EntityType = EntityAsset
	case kindSigner:
		id.EntityType = EntitySigner
	case kindService:
		// A service account is a machine user (AAA contract D17). Cloud encodes
		// the entity it relayed in the name; anything else is a plain service
		// account and reads as a user.
		switch {
		case strings.HasPrefix(name, "asset-"):
			id.EntityType, id.EntityID = EntityAsset, strings.TrimPrefix(name, "asset-")
		case strings.HasPrefix(name, "user-"):
			id.EntityType, id.EntityID = EntityUser, strings.TrimPrefix(name, "user-")
		default:
			id.EntityType = EntityUser
		}
		if id.EntityID == "" {
			return Principal{}, fmt.Errorf("SPIFFE principal has empty entity id: %s", principal)
		}
	default:
		return Principal{}, fmt.Errorf("unknown SPIFFE principal kind %q: %s", kind, principal)
	}
	return id, nil
}
