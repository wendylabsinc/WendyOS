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

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
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
	// means "build the CSR the pre-tenant way", never an error (WDY-2584).
	TenantUUID string `json:"tenant_uuid"`
}

// TenantSPIFFEURI returns the tenant SPIFFE principal these claims describe,
// and whether one could be built at all.
//
// Cloud's fabric relay refuses to sign a grant unless the CSR carries exactly
// this URI SAN, so every CSR built from an enrollment token has to ask. It
// returns ok=false when the token carries no tenant_uuid (the org has no pki
// tenant) or when the claims lack the entity ID the principal needs — both are
// ordinary states, so callers omit the SAN and carry on rather than failing.
func (c Claims) TenantSPIFFEURI() (uri string, ok bool) {
	if c.TenantUUID == "" {
		return "", false
	}
	switch c.Type {
	case "asset_enrollment":
		if c.AssetID == 0 {
			return "", false
		}
		return certs.AssetSPIFFEURI(c.TenantUUID, c.AssetID), true
	case "user_enrollment":
		if c.UserID == "" {
			return "", false
		}
		return certs.UserSPIFFEURI(c.TenantUUID, c.UserID), true
	default:
		return "", false
	}
}

// TenantSPIFFEURIFromToken is TenantSPIFFEURI for callers that hold the raw
// token rather than decoded claims. A token that will not decode yields
// ok=false: the CSR is built without the SAN and the cloud rejects it there,
// which is a clearer failure than one raised here.
func TenantSPIFFEURIFromToken(token string) (uri string, ok bool) {
	c, err := Parse(token)
	if err != nil {
		return "", false
	}
	return c.TenantSPIFFEURI()
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
