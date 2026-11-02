package localmesh

import (
	"errors"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// OperatorOwner gives one installation its own origin/replay identity while
// retaining the real operator certificate principal in SignedManifest.
type OperatorOwner struct {
	Principal string    `json:"principal" cbor:"p"`
	Instance  uuid.UUID `json:"instance" cbor:"n"`
	Hashes    [][]byte  `json:"hashes" cbor:"h"`
}

func (m Manifest) Peer(tenant meshid.Tenant) (meshid.Device, error) {
	if m.Owner == nil {
		return meshid.NewDevice(tenant, m.Device), nil
	}
	peer, err := meshid.PeerFromPrincipal(m.Owner.Principal)
	if err != nil {
		return meshid.Device{}, err
	}
	if !peer.IsOperator() || peer.Tenant() != tenant || m.Device != uuid.Nil {
		return meshid.Device{}, errors.New("invalid operator manifest owner")
	}
	if len(m.Owner.Hashes) == 0 || len(m.Owner.Hashes) > 16 {
		return meshid.Device{}, errors.New("operator owner requires its exact DER chain hashes")
	}
	for _, hash := range m.Owner.Hashes {
		if len(hash) != 32 {
			return meshid.Device{}, errors.New("invalid operator DER chain hash")
		}
	}
	return peer.WithInstance(m.Owner.Instance)
}

func (w SignedManifest) peer() (meshid.Device, error) {
	identity, err := meshid.PeerFromPrincipal(w.Principal)
	if err != nil {
		return meshid.Device{}, err
	}
	m, err := DecodeManifest(w.Body)
	if err != nil {
		return meshid.Device{}, err
	}
	peer, err := m.Peer(identity.Tenant())
	if err != nil {
		return meshid.Device{}, err
	}
	if peer.Principal() != w.Principal {
		return meshid.Device{}, errors.New("manifest signer and owner disagree")
	}
	return peer, nil
}

func (w SignedManifest) ownerKey() string {
	peer, err := w.peer()
	if err != nil {
		return ""
	}
	return peer.Key()
}
