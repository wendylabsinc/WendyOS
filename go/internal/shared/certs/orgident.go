// Package certs provides certificate and key utilities for mTLS authentication.
package certs

import (
	"crypto/x509"
	"fmt"
	"strconv"
	"strings"
)

const wendyOrgURNPrefix = "urn:wendy:org:"

// WendyIdentity holds the Wendy org and entity identity extracted from a certificate.
type WendyIdentity struct {
	OrgID      int32
	EntityType string // "user" or "asset"
	EntityID   string // numeric ID as string
}

// IdentityKey returns the canonical URN string used as a pin-store key.
func (w WendyIdentity) IdentityKey() string {
	return fmt.Sprintf("urn:wendy:org:%d:%s:%s", w.OrgID, w.EntityType, w.EntityID)
}

// UserURN returns the canonical Wendy identity URN for a user:
// "urn:wendy:org:<org>:user:<userID>". This is the URI SAN a user (CLI)
// certificate carries as its authoritative identity.
func UserURN(orgID int32, userID string) string {
	return WendyIdentity{OrgID: orgID, EntityType: "user", EntityID: userID}.IdentityKey()
}

// AssetURN returns the canonical Wendy identity URN for an asset:
// "urn:wendy:org:<org>:asset:<assetID>". This is the URI SAN a device (agent)
// certificate carries as its authoritative identity.
func AssetURN(orgID, assetID int32) string {
	return WendyIdentity{OrgID: orgID, EntityType: "asset", EntityID: strconv.Itoa(int(assetID))}.IdentityKey()
}

// TenantSPIFFEPrefix is the trust domain and tenant path every pki-core
// principal is minted under. Taken from the tenantSPIFFEPrefix constant on
// origin/sem/wdy-2899-acme-enrollment, exported here because the device
// enrolment client has to recognise the prefix as well as build it.
//
// Note the kind that follows the tenant segment differs by issuance path, and
// the difference is load-bearing. Wendy Cloud relays a client leaf through
// pki-core's "service-identity" profile, so a cloud-minted principal is
// ".../service/asset-<id>". A leaf enrolled directly against pki-core's device
// profile is ".../device/<name>", and the Wendy Data Platform's ingest
// interceptor rejects any kind other than "device" outright. The two are
// separate identities on separate certificates, not two spellings of one.
const TenantSPIFFEPrefix = "spiffe://wendy.sh/tenant/"

// spiffeDeviceKind is the principal kind pki-core stamps for a leaf issued
// from one of its device tiers.
const spiffeDeviceKind = "device"

// DeviceSPIFFEURI returns the canonical pki-core device principal:
// "spiffe://wendy.sh/tenant/<tenantUUID>/device/<deviceName>".
//
// It is built here only so the agent can state what it expects and compare.
// pki-core stamps this SAN server-side from the enrollment token's device_id
// and discards whatever URI SANs the CSR carried, so nothing the device puts in
// a CSR can influence the issued identity. deviceName may be multi-segment
// (for example "sh/wendy/2/408"); each "/"-separated segment must match
// ^[A-Za-z0-9._-]{1,64}$.
func DeviceSPIFFEURI(tenantUUID, deviceName string) string {
	return TenantSPIFFEPrefix + tenantUUID + "/" + spiffeDeviceKind + "/" + deviceName
}

// ParseDeviceSPIFFEURI splits a pki-core device principal back into its tenant
// UUID and device name. A principal of any other kind ("service", "operator")
// is an error rather than a miss, because accepting one where a device identity
// is required is precisely the mistake this function exists to prevent.
func ParseDeviceSPIFFEURI(uri string) (tenantUUID, deviceName string, err error) {
	rest, ok := strings.CutPrefix(uri, TenantSPIFFEPrefix)
	if !ok {
		return "", "", fmt.Errorf("not a tenant SPIFFE URI: %s", uri)
	}
	tenantUUID, rest, ok = strings.Cut(rest, "/")
	if !ok || tenantUUID == "" {
		return "", "", fmt.Errorf("tenant SPIFFE URI carries no tenant segment: %s", uri)
	}
	kind, name, ok := strings.Cut(rest, "/")
	if !ok || name == "" {
		return "", "", fmt.Errorf("tenant SPIFFE URI carries no %s name: %s", spiffeDeviceKind, uri)
	}
	if kind != spiffeDeviceKind {
		return "", "", fmt.Errorf("tenant SPIFFE URI is a %q principal, not a %s: %s", kind, spiffeDeviceKind, uri)
	}
	return tenantUUID, name, nil
}

// TenantSPIFFEURIs returns every tenant SPIFFE URI SAN on leaf, in the order
// the certificate carries them. Callers that require a single identity check
// the length themselves so they can report how many were found.
func TenantSPIFFEURIs(leaf *x509.Certificate) []string {
	var out []string
	for _, u := range leaf.URIs {
		raw := u.String()
		if strings.HasPrefix(raw, TenantSPIFFEPrefix) {
			out = append(out, raw)
		}
	}
	return out
}

// ParseIdentityURN parses a canonical Wendy identity URN —
// "urn:wendy:org:<org>:(user|asset):<id>", the exact string IdentityKey
// produces — back into a WendyIdentity.
//
// It exists because that URN is user-facing: it is the key the device pin store
// is filed under and the key an SPKI refusal prints, so `wendy device unpin`
// has to accept it as an argument. Parsing goes through the same
// parseWendyOrgURN the certificate path uses, so what the CLI accepts from a
// user and what it reads out of a certificate can never drift apart — a second
// hand-rolled parser here would be a second definition of what a Wendy identity
// is.
func ParseIdentityURN(urn string) (WendyIdentity, error) {
	return parseWendyOrgURN(strings.TrimSpace(urn))
}

// IdentityFromCert extracts the Wendy org+entity identity from a certificate.
//
// Resolution order:
//  1. SAN URI beginning with "urn:wendy:org:" (authoritative; exactly one allowed)
//  2. CommonName "sh/wendy/<org>/<asset>" (legacy fallback)
//  3. No identity: returns (zero, false, nil)
func IdentityFromCert(leaf *x509.Certificate) (WendyIdentity, bool, error) {
	var wendyURNs []string
	for _, u := range leaf.URIs {
		raw := u.String()
		if strings.HasPrefix(raw, wendyOrgURNPrefix) {
			wendyURNs = append(wendyURNs, raw)
		}
	}
	if len(wendyURNs) > 1 {
		return WendyIdentity{}, false, fmt.Errorf("certificate contains %d wendy org URNs; expected at most one", len(wendyURNs))
	}
	if len(wendyURNs) == 1 {
		id, err := parseWendyOrgURN(wendyURNs[0])
		if err != nil {
			return WendyIdentity{}, false, err
		}
		return id, true, nil
	}

	cn := leaf.Subject.CommonName
	if strings.HasPrefix(cn, "sh/wendy/") {
		id, err := parseShWendyCN(cn)
		if err != nil {
			return WendyIdentity{}, false, err
		}
		return id, true, nil
	}

	return WendyIdentity{}, false, nil
}

// OrgFromClientCert extracts the org ID from a certificate. It is a wrapper
// around IdentityFromCert that drops entity type and ID.
func OrgFromClientCert(leaf *x509.Certificate) (orgID int32, hasOrg bool, err error) {
	id, ok, err := IdentityFromCert(leaf)
	return id.OrgID, ok, err
}

// parseWendyOrgURN parses "urn:wendy:org:<org>:(user|asset):<id>" into a WendyIdentity.
func parseWendyOrgURN(uri string) (WendyIdentity, error) {
	parts := strings.Split(uri, ":")
	if len(parts) != 6 {
		return WendyIdentity{}, fmt.Errorf("invalid wendy URN format (want 6 colon-separated parts): %s", uri)
	}
	if parts[0] != "urn" || parts[1] != "wendy" || parts[2] != "org" {
		return WendyIdentity{}, fmt.Errorf("invalid wendy URN prefix: %s", uri)
	}
	orgID, err := strconv.ParseInt(parts[3], 10, 32)
	if err != nil {
		return WendyIdentity{}, fmt.Errorf("invalid organization ID in URN %q: %w", parts[3], err)
	}
	if orgID <= 0 {
		return WendyIdentity{}, fmt.Errorf("organization ID must be positive, got %d", orgID)
	}
	entityType := parts[4]
	if entityType != "user" && entityType != "asset" {
		return WendyIdentity{}, fmt.Errorf("unknown entity type in wendy URN %q: %s", uri, entityType)
	}
	if parts[5] == "" {
		return WendyIdentity{}, fmt.Errorf("empty entity ID in wendy URN: %s", uri)
	}
	return WendyIdentity{OrgID: int32(orgID), EntityType: entityType, EntityID: parts[5]}, nil
}

// parseShWendyCN parses "sh/wendy/<org>/<asset>" into a WendyIdentity.
// Caller must have verified the CN starts with "sh/wendy/".
func parseShWendyCN(cn string) (WendyIdentity, error) {
	parts := strings.Split(cn, "/")
	if len(parts) != 4 {
		return WendyIdentity{}, fmt.Errorf("invalid sh/wendy CommonName (want 4 slash-separated segments): %s", cn)
	}
	orgID, err := strconv.ParseInt(parts[2], 10, 32)
	if err != nil {
		return WendyIdentity{}, fmt.Errorf("invalid organization ID in CommonName %q: %w", parts[2], err)
	}
	if orgID <= 0 {
		return WendyIdentity{}, fmt.Errorf("organization ID must be positive, got %d", orgID)
	}
	if parts[3] == "" {
		return WendyIdentity{}, fmt.Errorf("empty asset ID in CommonName: %s", cn)
	}
	return WendyIdentity{OrgID: int32(orgID), EntityType: "asset", EntityID: parts[3]}, nil
}
