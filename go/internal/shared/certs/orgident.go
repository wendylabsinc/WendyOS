// Package certs provides certificate and key utilities for mTLS authentication.
package certs

import (
	"crypto/x509"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

const wendyOrgURNPrefix = "urn:wendy:org:"

// tenantSPIFFEPrefix is the trust domain and tenant path pki-core mints every
// current-chain leaf under: "spiffe://wendy.sh/tenant/<uuid>/<kind>/<name>".
const tenantSPIFFEPrefix = "spiffe://wendy.sh/tenant/"

// Principal kinds pki-core mints under a tenant, per the AAA contract §4.2/D17:
// operator (a human), service (a service account — a machine user privileged
// exactly as a human), device (a device leaf), signer (code signing, not an
// actor). Cloud additionally spells its relayed leaves "service/user-<id>" and
// "service/asset-<id>", which parse into the same user/asset entity types the
// legacy URN produced.
const (
	kindOperator = "operator"
	kindService  = "service"
	kindDevice   = "device"
	kindSigner   = "signer"
)

// EntityType values a WendyIdentity can carry. "signer" exists so a code-signing
// leaf parses into something nameable and is then refused by the actor gates,
// rather than being mistaken for an operator.
const (
	EntityUser   = "user"
	EntityAsset  = "asset"
	EntitySigner = "signer"
)

// Scope is the tenant an identity belongs to.
//
// pki-core is the identity authority and scopes everything it mints by tenant
// UUID; the int32 org survives only on old-chain certificates that predate
// tenant SPIFFE SANs. Both are carried because a transitional leaf presents
// both SANs, and a comparison is only meaningful between two scopes expressed
// in the same terms.
type Scope struct {
	TenantUUID string
	OrgID      int32
}

// Known reports whether this scope names anything at all.
func (s Scope) Known() bool { return s.TenantUUID != "" || s.OrgID > 0 }

// Matches reports whether two scopes provably name the same tenant.
//
// Tenant UUIDs are compared when both sides have one; otherwise the legacy org
// is compared when both sides have one. A scope pair with no shared vocabulary
// — a SPIFFE-only peer against an org-only expectation — is NOT a match: there
// is no mapping between a tenant UUID and an int32 org, and answering "equal"
// on an unprovable pair is exactly the silent disarming this replaces.
func (s Scope) Matches(o Scope) bool {
	if s.TenantUUID != "" && o.TenantUUID != "" {
		return s.TenantUUID == o.TenantUUID
	}
	if s.OrgID > 0 && o.OrgID > 0 {
		return s.OrgID == o.OrgID
	}
	return false
}

// Comparable reports whether two scopes are expressed in a shared vocabulary.
//
// It is what separates "a different tenant" from "no way to tell": Matches
// returns false for both, but only the first is a cross-tenant attempt. A
// caller that offers a migration grace period needs to forgive the second
// without forgiving the first.
func (s Scope) Comparable(o Scope) bool {
	return (s.TenantUUID != "" && o.TenantUUID != "") || (s.OrgID > 0 && o.OrgID > 0)
}

// String renders a scope for logs. Neither half is PII.
func (s Scope) String() string {
	switch {
	case s.TenantUUID != "" && s.OrgID > 0:
		return fmt.Sprintf("tenant %s (org %d)", s.TenantUUID, s.OrgID)
	case s.TenantUUID != "":
		return "tenant " + s.TenantUUID
	case s.OrgID > 0:
		return fmt.Sprintf("org %d", s.OrgID)
	default:
		return "no scope"
	}
}

// WendyIdentity holds the identity extracted from a certificate.
//
// Principal/TenantUUID are the authoritative pair — the tenant SPIFFE SAN
// pki-core stamps on every leaf it issues or renews. OrgID/EntityType/EntityID
// are the legacy urn:wendy reading, present only on old chains and on the
// transitional leaves cloud mints carrying both SANs.
type WendyIdentity struct {
	OrgID      int32
	EntityType string // "user", "asset" or "signer"
	EntityID   string // user/asset id, or the device id for a device principal
	TenantUUID string // pki-core tenant; "" on an old-chain certificate
	Principal  string // full spiffe:// URI; "" on an old-chain certificate
}

// Scope returns the tenant this identity belongs to.
func (w WendyIdentity) Scope() Scope {
	return Scope{TenantUUID: w.TenantUUID, OrgID: w.OrgID}
}

// IdentityKey returns the canonical string used as a pin-store key: the SPIFFE
// principal when the certificate carries one, and the legacy URN otherwise.
//
// The principal is the durable choice — pki-core carries it across every
// renewal by construction, whereas the URN is dropped by any renewal — so a pin
// filed under it survives the certificate lifecycle the URN does not.
func (w WendyIdentity) IdentityKey() string {
	if w.Principal != "" {
		return w.Principal
	}
	return w.LegacyURN()
}

// LegacyURN returns the urn:wendy identity URN this identity would have carried
// on an old chain, or "" when it has no legacy org reading. It is what a pin
// written before the SPIFFE cutover is filed under.
func (w WendyIdentity) LegacyURN() string {
	if w.OrgID <= 0 || w.EntityType == "" || w.EntityID == "" {
		return ""
	}
	return fmt.Sprintf("urn:wendy:org:%d:%s:%s", w.OrgID, w.EntityType, w.EntityID)
}

// SameEntity reports whether two identities provably name the same principal.
//
// Principals are compared when both sides have one; otherwise the legacy
// scope+type+id triple must match. As with Scope.Matches, a pair with no shared
// vocabulary is not a match.
func (w WendyIdentity) SameEntity(o WendyIdentity) bool {
	if w.Principal != "" && o.Principal != "" {
		return w.Principal == o.Principal
	}
	if w.EntityType != o.EntityType || w.EntityID == "" || w.EntityID != o.EntityID {
		return false
	}
	return w.Scope().Matches(o.Scope())
}

// UserURN returns the legacy Wendy identity URN for a user:
// "urn:wendy:org:<org>:user:<userID>".
func UserURN(orgID int32, userID string) string {
	return WendyIdentity{OrgID: orgID, EntityType: EntityUser, EntityID: userID}.LegacyURN()
}

// AssetURN returns the legacy Wendy identity URN for an asset:
// "urn:wendy:org:<org>:asset:<assetID>".
func AssetURN(orgID, assetID int32) string {
	return WendyIdentity{OrgID: orgID, EntityType: EntityAsset, EntityID: strconv.Itoa(int(assetID))}.LegacyURN()
}

// AssetSPIFFEURI returns the tenant SPIFFE principal cloud mints for a device
// it relays through the service-identity profile:
// "spiffe://wendy.sh/tenant/<tenantUUID>/service/asset-<assetID>".
//
// Cloud refuses to sign a grant unless the CSR carries exactly this URI SAN
// (WDY-2498/WDY-2584). A device enrolled directly against pki-core over
// ACME/EST gets "device/<deviceID>" instead; both parse to an asset entity.
func AssetSPIFFEURI(tenantUUID string, assetID int32) string {
	return tenantSPIFFEPrefix + tenantUUID + "/service/asset-" + strconv.Itoa(int(assetID))
}

// UserSPIFFEURI returns the tenant SPIFFE principal cloud mints for a user:
// "spiffe://wendy.sh/tenant/<tenantUUID>/service/user-<userID>". A cert issued
// by pki-core's own operator identity endpoint carries "operator/<sub>"; both
// parse to a user entity.
func UserSPIFFEURI(tenantUUID, userID string) string {
	return tenantSPIFFEPrefix + tenantUUID + "/service/user-" + userID
}

// DeviceSPIFFEURI returns the tenant SPIFFE principal pki-core stamps on a leaf
// enrolled over ACME or EST: "spiffe://wendy.sh/tenant/<tenantUUID>/device/<deviceID>".
// The device id is path-shaped and may carry slashes.
func DeviceSPIFFEURI(tenantUUID, deviceID string) string {
	return tenantSPIFFEPrefix + tenantUUID + "/" + kindDevice + "/" + deviceID
}

// TenantPrincipalFromCert returns the tenant SPIFFE principal a leaf carries,
// and whether it carries exactly one.
//
// pki-core routes a renewal by this SAN alone — it reads the tenant out of the
// certificate presented in the handshake rather than from the request — and
// refuses a certificate that presents none or several. So "exactly one" is the
// only answer that means renewable.
func TenantPrincipalFromCert(leaf *x509.Certificate) (string, bool) {
	var found string
	for _, u := range leaf.URIs {
		raw := u.String()
		if !strings.HasPrefix(raw, tenantSPIFFEPrefix) {
			continue
		}
		if found != "" {
			return "", false
		}
		found = raw
	}
	return found, found != ""
}

// ParsePrincipal parses a tenant SPIFFE principal —
// "spiffe://wendy.sh/tenant/<uuid>/<kind>/<name>" — into a WendyIdentity.
//
// Every kind pki-core mints is accepted, because the parser is the one place
// that decides what a Wendy identity is and a kind it refuses is an actor no
// gate can see. The name of a device principal is path-shaped and keeps its
// slashes; cloud's "service/user-<id>" and "service/asset-<id>" spellings are
// unwrapped to the user/asset entity types the rest of the code compares on.
func ParsePrincipal(principal string) (WendyIdentity, error) {
	rest, ok := strings.CutPrefix(principal, tenantSPIFFEPrefix)
	if !ok {
		return WendyIdentity{}, fmt.Errorf("not a wendy tenant SPIFFE principal: %s", principal)
	}
	tenant, kindAndName, ok := strings.Cut(rest, "/")
	if !ok {
		return WendyIdentity{}, fmt.Errorf("SPIFFE principal has no kind: %s", principal)
	}
	// pki-core routes by the tenant UUID and compares it canonically, so a
	// non-canonical spelling of the same tenant is a different string to every
	// downstream comparison. Reject it here rather than let two spellings of one
	// tenant fail to match each other.
	if parsed, err := uuid.Parse(tenant); err != nil || parsed.String() != tenant {
		return WendyIdentity{}, fmt.Errorf("SPIFFE principal has non-canonical tenant UUID: %s", principal)
	}
	kind, name, ok := strings.Cut(kindAndName, "/")
	if !ok || name == "" {
		return WendyIdentity{}, fmt.Errorf("SPIFFE principal has no name: %s", principal)
	}

	id := WendyIdentity{TenantUUID: tenant, Principal: principal, EntityID: name}
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
			return WendyIdentity{}, fmt.Errorf("SPIFFE principal has empty entity id: %s", principal)
		}
	default:
		return WendyIdentity{}, fmt.Errorf("unknown SPIFFE principal kind %q: %s", kind, principal)
	}
	return id, nil
}

// ParseIdentityURN parses either identity string a refusal can print — a tenant
// SPIFFE principal or a legacy "urn:wendy:org:<org>:(user|asset):<id>" URN —
// back into a WendyIdentity.
//
// It exists because those strings are user-facing: one of them is the key the
// device pin store is filed under and the key an SPKI refusal prints, so
// `wendy device unpin` has to accept it as an argument. Both forms go through
// the same parsers the certificate path uses, so what the CLI accepts from a
// user and what it reads out of a certificate can never drift apart.
func ParseIdentityURN(urn string) (WendyIdentity, error) {
	s := strings.TrimSpace(urn)
	if strings.HasPrefix(s, tenantSPIFFEPrefix) {
		return ParsePrincipal(s)
	}
	return parseWendyOrgURN(s)
}

// IdentityFromCert extracts the Wendy identity from a certificate.
//
// Resolution order, SPIFFE first:
//
//  1. The tenant SPIFFE SAN "spiffe://wendy.sh/tenant/<uuid>/<kind>/<name>" —
//     authoritative, exactly one allowed. This is what pki-core stamps on
//     everything it issues and re-stamps on everything it renews.
//  2. SAN URI "urn:wendy:org:<org>:..." — legacy old-chain reading, exactly one
//     allowed. When a leaf carries both (the transitional shape cloud mints),
//     the URN contributes only its org, so an old-chain peer comparing orgs
//     still matches; the principal decides who the caller is.
//  3. CommonName "sh/wendy/<org>/<asset>" — legacy old-chain fallback.
//  4. No identity: returns (zero, false, nil).
func IdentityFromCert(leaf *x509.Certificate) (WendyIdentity, bool, error) {
	var principals, wendyURNs []string
	for _, u := range leaf.URIs {
		switch raw := u.String(); {
		case strings.HasPrefix(raw, tenantSPIFFEPrefix):
			principals = append(principals, raw)
		case strings.HasPrefix(raw, wendyOrgURNPrefix):
			wendyURNs = append(wendyURNs, raw)
		}
	}
	if len(principals) > 1 {
		return WendyIdentity{}, false, fmt.Errorf("certificate contains %d tenant SPIFFE principals; expected at most one", len(principals))
	}
	if len(wendyURNs) > 1 {
		return WendyIdentity{}, false, fmt.Errorf("certificate contains %d wendy org URNs; expected at most one", len(wendyURNs))
	}

	var legacy WendyIdentity
	if len(wendyURNs) == 1 {
		parsed, err := parseWendyOrgURN(wendyURNs[0])
		if err != nil {
			return WendyIdentity{}, false, err
		}
		legacy = parsed
	}

	if len(principals) == 1 {
		id, err := ParsePrincipal(principals[0])
		if err != nil {
			return WendyIdentity{}, false, err
		}
		// The legacy URN survives only as the org it names, so a peer that can
		// still only compare orgs keeps working against this leaf — but only
		// when the two SANs agree about which entity this is. A leaf whose URN
		// names a different entity than its principal is misissued; the
		// principal is authoritative, so the contradictory legacy claim
		// contributes nothing rather than lending its org to an entity it does
		// not describe.
		if legacy.EntityType == "" || (legacy.EntityType == id.EntityType && legacy.EntityID == id.EntityID) {
			id.OrgID = legacy.OrgID
		}
		return id, true, nil
	}

	if legacy.EntityType != "" {
		return legacy, true, nil
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

// ScopeFromCert extracts the tenant scope a certificate belongs to. It is a
// wrapper around IdentityFromCert that drops the entity.
func ScopeFromCert(leaf *x509.Certificate) (scope Scope, known bool, err error) {
	id, ok, err := IdentityFromCert(leaf)
	if err != nil || !ok {
		return Scope{}, false, err
	}
	return id.Scope(), id.Scope().Known(), nil
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
	if entityType != EntityUser && entityType != EntityAsset {
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
	return WendyIdentity{OrgID: int32(orgID), EntityType: EntityAsset, EntityID: parts[3]}, nil
}

// ScopeFromCertPEM parses a PEM-encoded leaf (ML-DSA aware, via
// ParseCertsFromPEM) and returns the tenant scope it belongs to, or
// (zero, false) when the PEM is unparseable or carries no identity.
//
// It is how a device answers "which tenant am I?" about its own certificate,
// which is the value every peer's scope is compared against.
func ScopeFromCertPEM(certPEM string) (Scope, bool) {
	if certPEM == "" {
		return Scope{}, false
	}
	parsed, err := ParseCertsFromPEM([]byte(certPEM))
	if err != nil || len(parsed) == 0 {
		return Scope{}, false
	}
	scope, known, err := ScopeFromCert(parsed[0])
	if err != nil {
		return Scope{}, false
	}
	return scope, known
}
