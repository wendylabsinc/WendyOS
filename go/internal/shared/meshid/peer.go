package meshid

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"net/netip"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// PeerFromPrincipal extends the router's peer vocabulary with a real operator
// role. DeviceFromPrincipal deliberately remains device-only. A certificate
// establishes the principal; the authenticated hello and signed manifest bind
// the installation UUID before an operator address can be used.
// Adapted from Joannis Orlandos's PR #2466, commit 957fb0bf3.
func PeerFromPrincipal(principal string) (Device, error) {
	if d, err := DeviceFromPrincipal(principal); err == nil {
		return d, nil
	}
	id, err := certs.ParsePrincipal(principal)
	if err != nil {
		return Device{}, err
	}
	tenant, err := ParseTenant(id.TenantUUID)
	if err != nil {
		return Device{}, err
	}
	if id.EntityType != certs.EntityUser || id.EntityID == "" || principal != "spiffe://wendy.sh/tenant/"+tenant.String()+"/operator/"+id.EntityID {
		return Device{}, errors.New("mesh peer must be a device or exact tenant operator")
	}
	u, err := url.Parse(principal)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.String() != principal || len(principal) > 1024 {
		return Device{}, errors.New("operator principal is not canonical URI text")
	}
	for _, c := range principal {
		if c < 0x20 || c == 0x7f {
			return Device{}, errors.New("invalid operator principal")
		}
	}
	return Device{tenant: tenant, operator: principal}, nil
}

func PeerFromCert(leaf *x509.Certificate) (Device, error) {
	id, found, err := certs.IdentityFromCert(leaf)
	if err != nil {
		return Device{}, err
	}
	if !found || id.Principal == "" {
		return Device{}, errors.New("mesh peer lacks a tenant principal")
	}
	return PeerFromPrincipal(id.Principal)
}

func (d Device) IsOperator() bool { return d.operator != "" }

// WithInstance binds one operator installation without changing its actual
// certificate principal. Device UUIDs cannot be replaced by this operation.
func (d Device) WithInstance(instance uuid.UUID) (Device, error) {
	if !d.IsOperator() || instance == uuid.Nil {
		return Device{}, errors.New("operator requires an installation UUID")
	}
	d.id = instance
	return d, nil
}

func (d Device) operatorAddress() netip.Addr {
	if d.id == uuid.Nil {
		return netip.Addr{}
	}
	h := sha256.New()
	h.Write([]byte("wendy-net/operator-address/v1\x00"))
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(d.operator)))
	h.Write(length[:])
	h.Write([]byte(d.operator))
	h.Write(d.id[:])
	var out [16]byte
	copy(out[:6], []byte{0xfd, 0x45, 0xb8, 0x91, 0x56, 0x80})
	copy(out[6:], h.Sum(nil)[:10])
	return netip.AddrFrom16(out)
}

// Key is an internal record-owner key, never a certificate principal. Multiple
// installations share a signing principal but have independent replay floors.
func (d Device) Key() string {
	if !d.IsOperator() {
		return d.Principal()
	}
	return d.Principal() + "#" + d.id.String()
}

func PeerFromKey(key string) (Device, error) {
	principal, instance, has := strings.Cut(key, "#")
	d, err := PeerFromPrincipal(principal)
	if err != nil {
		return Device{}, err
	}
	if !d.IsOperator() {
		if has {
			return Device{}, errors.New("device key has instance")
		}
		return d, nil
	}
	if !has {
		return Device{}, errors.New("operator record key lacks instance")
	}
	id, err := parseCanonical(instance)
	if err != nil {
		return Device{}, err
	}
	return d.WithInstance(id)
}
