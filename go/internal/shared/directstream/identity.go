package directstream

import "crypto/sha256"

// EndpointIdentity contains hashes computed locally from authenticated material:
// exact canonical principal URI and DER SubjectPublicKeyInfo. Instance is the
// device UUID or authenticated operator installation UUID; Role is device0 or
// operator1, distinct from the logical coordinator/participant ordering.
// Local credential/trust policy epochs are not peer-controlled wire counters:
// changing either must destroy the retained session authority. Its original
// hard security lifetime is never renewed by attachment or TLS resumption.
type EndpointIdentity struct {
	Principal, Key [32]byte
	Instance       [16]byte
	Role           byte
}

func (i EndpointIdentity) valid() bool {
	return i.Role <= 1 && i.Principal != ([32]byte{}) && i.Key != ([32]byte{}) && i.Instance != ([16]byte{})
}

// IdentityDigest uses coordinator-first order selected from full authenticated
// principals/UUIDs. Do not sort hashes or accept these fields from an OFFER.
func IdentityDigest(coordinator, participant EndpointIdentity) ([32]byte, error) {
	if !coordinator.valid() || !participant.valid() || (coordinator.Principal == participant.Principal && coordinator.Instance == participant.Instance) {
		return [32]byte{}, ErrBounds
	}
	h := sha256.New()
	h.Write([]byte("wendy/direct/identities/1\x00"))
	for _, i := range []EndpointIdentity{coordinator, participant} {
		h.Write([]byte{i.Role})
		h.Write(i.Principal[:])
		h.Write(i.Instance[:])
		h.Write(i.Key[:])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}
