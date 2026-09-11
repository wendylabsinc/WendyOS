// Package enrolltoken decodes the (unverified) claims payload of a Wendy
// enrollment token. It never validates the signature — that is the cloud's
// job at certificate-issuance time. It exists so the CLI and the agent derive
// org/asset identity from a token identically.
package enrolltoken

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Claims holds the fields Wendy embeds in an enrollment token payload.
type Claims struct {
	OrganizationID int32  `json:"org_id"`
	AssetID        int32  `json:"asset_id"`
	UserID         string `json:"user_id"`
	Type           string `json:"type"`
	// TenantUUID is the org's pki-core tenant, as a lowercase canonical UUID.
	// It is OPTIONAL: cloud omits it for organizations with no pki tenant,
	// which is the normal state for the local and GCP CAS backends. Absence
	// means "the caller has to be told the tenant another way", never an error
	// (WDY-2584).
	//
	// The field and this reasoning are taken from
	// origin/sem/wdy-2899-acme-enrollment, which introduced the claim.
	TenantUUID string `json:"tenant_uuid"`
}

// TenantUUIDFromToken returns the tenant_uuid claim carried by an enrollment
// token, and whether one was there at all.
//
// ok=false covers two ordinary states and does not distinguish them, because a
// caller can do nothing different about either: a Wendy-minted token for an org
// with no pki tenant, and a token that is not a Wendy JWT in the first place
// (pki-core's own enrollment tokens are opaque values with no claims to read).
// Both mean "take the tenant from the enrol input instead".
func TenantUUIDFromToken(token string) (tenantUUID string, ok bool) {
	c, err := Parse(token)
	if err != nil || c.TenantUUID == "" {
		return "", false
	}
	return c.TenantUUID, true
}

// Parse decodes the base64url JSON payload (the second dot-separated segment)
// of an enrollment token. It does not verify the signature.
func Parse(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return Claims{}, fmt.Errorf("invalid enrollment token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, fmt.Errorf("decoding token payload: %w", err)
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, fmt.Errorf("decoding token claims: %w", err)
	}
	return c, nil
}

// ParseAsset decodes an asset-enrollment token and returns its org and asset
// IDs. It errors on any other token type or missing IDs.
func ParseAsset(token string) (orgID, assetID int32, err error) {
	c, err := Parse(token)
	if err != nil {
		return 0, 0, err
	}
	if c.Type != "asset_enrollment" {
		return 0, 0, fmt.Errorf("not an asset enrollment token (type %q)", c.Type)
	}
	if c.OrganizationID == 0 || c.AssetID == 0 {
		return 0, 0, fmt.Errorf("asset enrollment token missing org_id or asset_id")
	}
	return c.OrganizationID, c.AssetID, nil
}
