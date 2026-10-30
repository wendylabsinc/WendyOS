package nanprovider

import (
	"encoding/hex"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshdiscovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func parseEvent(raw string) (string, map[string]string) {
	if i := strings.Index(raw, "NAN-"); i >= 0 {
		raw = raw[i:]
	}
	fields := strings.Fields(raw)
	values := map[string]string{}
	if len(fields) == 0 {
		return "", values
	}
	for _, field := range fields[1:] {
		k, v, ok := strings.Cut(field, "=")
		if ok {
			values[k] = v
		}
	}
	return fields[0], values
}

const (
	presenceMagic       = "WNAN"
	presenceVersion     = 1
	presenceMeshPresent = 1
	presenceBaseSize    = 38 // magic, version, flags, tenant UUID, device UUID
	presenceMeshSize    = presenceBaseSize + len(meshdiscovery.Hint{})
	maxPresenceSize     = 255
)

var errPresence = errors.New("invalid NAN peer presence")

// Version 2 adds the complete operator principal after the fixed fields; its
// routing UUID must match the principal-derived endpoint. Device advertisements
// retain version 1 for compatibility.
// presence carries a complete device principal without its fixed SPIFFE URI
// template. UUIDs use their standard 16-byte network-order representation.
// Mesh is optional; nil means no mesh participation, not an all-zero hint.
// No discovery field grants trust before the enrolled certificate is checked.
type presence struct {
	Device meshid.Device
	Mesh   *meshdiscovery.Hint
}

func (p presence) marshalBinary() ([]byte, error) {
	if !p.Device.IsValid() {
		return nil, errPresence
	}
	size := presenceBaseSize
	if p.Mesh != nil {
		size = presenceMeshSize
	}
	if p.Device.IsOperator() {
		size += len(p.Device.Principal())
		if size > maxPresenceSize {
			return nil, errPresence
		}
	}
	b := make([]byte, size)
	copy(b[:4], presenceMagic)
	b[4] = presenceVersion
	if p.Mesh != nil {
		b[5] = presenceMeshPresent
		copy(b[38:], p.Mesh[:])
	}
	tenant, device := p.Device.Tenant().UUID(), p.Device.UUID()
	copy(b[6:22], tenant[:])
	copy(b[22:38], device[:])
	if p.Device.IsOperator() {
		b[4] = 2
		offset := presenceBaseSize
		if p.Mesh != nil {
			offset = presenceMeshSize
		}
		copy(b[offset:], p.Device.Principal())
	}
	return b, nil
}

func parsePresence(b []byte) (presence, error) {
	if len(b) < presenceBaseSize || len(b) > maxPresenceSize {
		return presence{}, errPresence
	}
	if string(b[:4]) != presenceMagic || (b[4] != presenceVersion && b[4] != 2) || b[5]&^byte(presenceMeshPresent) != 0 {
		return presence{}, errPresence
	}
	hasMesh := b[5]&presenceMeshPresent != 0
	baseSize := presenceBaseSize
	if hasMesh {
		baseSize = presenceMeshSize
	}
	if len(b) < baseSize || (b[4] == presenceVersion && len(b) != baseSize) || (b[4] == 2 && len(b) == baseSize) {
		return presence{}, errPresence
	}
	// Every 16-byte UUID, including the explicitly present nil UUID, is a valid
	// identity component under meshid's enrollment contract.
	tenant, err := meshid.ParseTenant(uuid.UUID(b[6:22]).String())
	if err != nil {
		return presence{}, errPresence
	}
	p := presence{Device: meshid.NewDevice(tenant, uuid.UUID(b[22:38]))}
	if hasMesh {
		hint := meshdiscovery.Hint(b[38:46])
		p.Mesh = &hint
	}
	if b[4] == 2 {
		peer, err := meshid.PeerFromPrincipal(string(b[baseSize:]))
		if err != nil || !peer.IsOperator() || peer.Tenant() != tenant || peer.UUID() != p.Device.UUID() {
			return presence{}, errPresence
		}
		p.Device = peer
	}
	return p, nil
}

// encodePresence uses hex only for the supplicant command transport. The radio
// carries the decoded 38 or 46 bytes, including binary UUIDs and mesh hint.
func encodePresence(p presence) (string, error) {
	b, err := p.marshalBinary()
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// decodePresence filters tenant and mesh scope before discovery can consume an
// NDP slot or enter peer selection. The same gate applies to NDP requests and
// counter proposals. The full principal is still pinned during QUIC mTLS.
func decodePresence(tenant meshid.Tenant, mesh meshdiscovery.Hint, ssi string) (meshid.Device, error) {
	if !tenant.IsValid() || len(ssi) < 2*presenceBaseSize || len(ssi) > 2*maxPresenceSize {
		return meshid.Device{}, errPresence
	}
	b, err := hex.DecodeString(ssi)
	if err != nil {
		return meshid.Device{}, errPresence
	}
	p, err := parsePresence(b)
	if err != nil || p.Device.Tenant() != tenant || p.Mesh == nil || *p.Mesh != mesh {
		return meshid.Device{}, errPresence
	}
	return p.Device, nil
}
