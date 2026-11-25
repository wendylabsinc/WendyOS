// Package meshid is the wendy net identity contract: how a device principal becomes
// a wendy net address, how devices are ordered, and what enrollment arbitrates on.
//
// The authoritative device identity is the complete SPIFFE principal
// "spiffe://wendy.sh/tenant/<tenant-uuid>/device/<device-uuid>". The wendy net
// address is the ULA pool's six bytes followed by the device UUID's final ten
// bytes, verbatim — no hashing, packing, bit removal, or UUID-version-specific
// transformation, and no hash fallback for non-UUID identities.
//
// The device UUID is the device's ONE id: Cloud adopts it as the asset id, the
// roster carries it once, and peers are verified against it. There is no
// separate asset id type.
package meshid

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
	"net/netip"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// MeshPrefix is the Wendy ULA allocation (RFC 4193), shared across managed
// tenants. It is the address pool and routing-policy boundary — not an
// on-link subnet to configure on every TUN, and never advertised as ordinary
// device reachability. Tenant isolation is an authentication and
// authorization responsibility, not a prefix-allocation one.
var MeshPrefix = netip.MustParsePrefix("fd45:b891:567f::/48")

var meshPrefixBytes = [...]byte{0xfd, 0x45, 0xb8, 0x91, 0x56, 0x7f}

// SuffixLen is the number of device UUID bytes enrollment must keep globally
// unique: the UUID's final ten bytes, which alone determine the wendy net address.
const SuffixLen = 10

var (
	linkLocalLower = netip.MustParseAddr("fe80::1")
	linkLocalUpper = netip.MustParseAddr("fe80::2")
)

// parseCanonical parses a UUID and requires the canonical textual form
// (lowercase, hyphenated) enforced at enrollment and API boundaries. Any UUID
// version or allocation scheme is accepted; there is no v4 requirement.
func parseCanonical(text string) (uuid.UUID, error) {
	id, err := uuid.Parse(text)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%q is not a UUID", text)
	}
	if id.String() != text {
		return uuid.Nil, fmt.Errorf("%q is not the canonical UUID text form", text)
	}
	return id, nil
}

// Tenant is an authenticated organization scope.
type Tenant struct {
	id    uuid.UUID
	valid bool // an explicitly parsed nil UUID is distinct from an absent tenant
}

// ParseTenant parses a canonical tenant UUID.
func ParseTenant(text string) (Tenant, error) {
	id, err := parseCanonical(text)
	if err != nil {
		return Tenant{}, fmt.Errorf("tenant UUID: %w", err)
	}
	return Tenant{id: id, valid: true}, nil
}

func (t Tenant) UUID() uuid.UUID { return t.id }
func (t Tenant) String() string {
	if !t.valid {
		return ""
	}
	return t.id.String()
}

// IsValid distinguishes a parsed tenant UUID (including nil) from the zero value.
func (t Tenant) IsValid() bool { return t.valid }

// Device is the routing API identity. Enrolled devices retain their exact UUID
// address contract; an explicit operator peer retains its actual certificate
// principal and binds a separate installation UUID/address through PeerFromPrincipal
// and WithInstance. DeviceFromPrincipal and DeviceFromCert remain device-only.
type Device struct {
	tenant   Tenant
	id       uuid.UUID
	operator string
}

// NewDevice combines an already authenticated tenant scope with a device
// UUID, e.g. a device UUID recovered from a discovery advertisement.
func NewDevice(tenant Tenant, id uuid.UUID) Device {
	if !tenant.IsValid() {
		return Device{}
	}
	return Device{tenant: tenant, id: id}
}

// ParseDeviceUUID validates a canonical device UUID carried in an existing
// Cloud asset_id wire field. It grants no tenant scope or peer authorization.
func ParseDeviceUUID(text string) (uuid.UUID, error) { return parseCanonical(text) }

// ParseDevice parses a canonical device UUID within a known tenant, e.g. from
// a roster entry or configured peer list.
func ParseDevice(tenant Tenant, text string) (Device, error) {
	if !tenant.IsValid() {
		return Device{}, errors.New("device requires a tenant UUID")
	}
	id, err := parseCanonical(text)
	if err != nil {
		return Device{}, fmt.Errorf("device UUID: %w", err)
	}
	return Device{tenant: tenant, id: id}, nil
}

// DeviceFromPrincipal parses a complete device principal. The complete device
// component must parse as a canonical UUID. Path-shaped PKI device names are
// valid generic PKI identities but are reported as unsupported for this wendy net
// scheme; nothing hashes them into an address.
func DeviceFromPrincipal(principal string) (Device, error) {
	identity, err := certs.ParseDevicePrincipal(principal)
	if err != nil {
		return Device{}, fmt.Errorf("not a PKI device principal: %w", err)
	}
	tenant, err := ParseTenant(identity.TenantUUID)
	if err != nil {
		return Device{}, err
	}
	id, err := parseCanonical(identity.EntityID)
	if err != nil {
		return Device{}, fmt.Errorf("device component %q of %s is not a canonical UUID: unsupported for wendy net", identity.EntityID, principal)
	}
	return Device{tenant: tenant, id: id}, nil
}

// DeviceFromCert extracts the wendy net device identity from a leaf certificate.
// It checks identity kind and shape only: certificate trust and the expected
// tenant scope must be verified by the caller before admission, and the full
// principal stays available for authorization. An IP address or router ID
// never replaces it.
func DeviceFromCert(leaf *x509.Certificate) (Device, error) {
	identity, found, err := certs.IdentityFromCert(leaf)
	if err != nil {
		return Device{}, err
	}
	if !found || identity.Principal == "" {
		return Device{}, errors.New("certificate carries no tenant SPIFFE principal")
	}
	return DeviceFromPrincipal(identity.Principal)
}

func (d Device) Tenant() Tenant  { return d.tenant }
func (d Device) UUID() uuid.UUID { return d.id }

// IsValid reports whether this identity has an explicit tenant and device UUID.
// Nil UUIDs are valid identifiers; they do not stand for missing enrollment.
func (d Device) IsValid() bool { return d.tenant.IsValid() }
func (d Device) Principal() string {
	if !d.IsValid() {
		return ""
	}
	if d.IsOperator() {
		return d.operator
	}
	return certs.DeviceSPIFFEURI(d.tenant.String(), d.id.String())
}
func (d Device) String() string { return d.Principal() }

// Suffix returns the device UUID's final ten bytes: the value enrollment
// arbitrates for global uniqueness under the shared /48, across tenants.
func (d Device) Suffix() [SuffixLen]byte {
	var s [SuffixLen]byte
	copy(s[:], d.id[6:])
	return s
}

// MeshAddressFor computes the destination address offline from a device UUID.
// Knowing a device UUID is sufficient to compute the address; Babel must still
// learn a route, so deterministic addressing does not imply reachability.
func MeshAddressFor(id uuid.UUID) netip.Addr {
	var b [16]byte
	copy(b[:6], meshPrefixBytes[:])
	copy(b[6:], id[6:])
	return netip.AddrFrom16(b)
}

// MeshAddress is the device's deterministic ULA, assigned /128 to the stable
// wendy net host interface and originated through Babel at metric zero.
func (d Device) MeshAddress() netip.Addr {
	if !d.IsValid() {
		return netip.Addr{}
	}
	if d.IsOperator() {
		return d.operatorAddress()
	}
	return MeshAddressFor(d.id)
}

// InMesh reports whether an address lies in the managed device pool.
func InMesh(a netip.Addr) bool {
	return a.Is6() && a.Zone() == "" && MeshPrefix.Contains(a)
}

// Compare orders devices by their complete 16-byte UUID as unsigned values in
// standard network byte order. Any pre-handshake connection-selection
// heuristic must converge on this ordering once the full identity is known.
func Compare(a, b Device) int {
	if a.IsOperator() || b.IsOperator() {
		if c := bytes.Compare([]byte(a.Principal()), []byte(b.Principal())); c != 0 {
			return c
		}
	}
	return bytes.Compare(a.id[:], b.id[:])
}

// LinkLocalAddresses assigns complementary link-local addresses for a
// point-to-point TUN from the full device UUIDs: lower UUID fe80::1, higher
// UUID fe80::2. Assignment is local — no DHCP, SLAAC, or negotiation — and
// the pair may repeat on other interfaces because next-hop scope includes the
// interface. Equal identities are a self/duplicate-identity connection, not
// two distinct routers.
func LinkLocalAddresses(self, peer Device) (netip.Addr, netip.Addr, error) {
	if !self.IsValid() || !peer.IsValid() {
		return netip.Addr{}, netip.Addr{}, errors.New("link requires two device identities")
	}
	switch Compare(self, peer) {
	case 0:
		return netip.Addr{}, netip.Addr{}, errors.New("self or duplicate device identity on link")
	case -1:
		return linkLocalLower, linkLocalUpper, nil
	default:
		return linkLocalUpper, linkLocalLower, nil
	}
}
