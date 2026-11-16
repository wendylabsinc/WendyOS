package directstream

import "errors"

// SessionBinding is RAM-only logical-session authority. Creation must generate
// SessionID/Capability cryptographically and derive IdentityDigest locally from
// authenticated principals, roles, instances and security epochs. TLS tickets
// are never session capabilities. Expiry/trust reset closes the generation gate.
type SessionBinding struct {
	SessionID, Capability, IdentityDigest [32]byte
	Role                                  byte
}

// Exporter must belong to the independently authenticated candidate connection
// represented by token, never a cached exporter from its previous incarnation.
type Exporter func(label string, context []byte, length int) ([]byte, error)

type RecoveryTransaction struct {
	gate                                 *GenerationGate
	token, generation                    uint64
	capability, exporter, context, proof [32]byte
	role                                 byte
	applied                              bool
}

var ErrAttachmentProof = errors.New("direct attachment proof rejected")

// PrepareRecoveryTransaction owns one attempt. Retransmissions reuse this object
// rather than reconstructing the authenticated transcript after application I/O advances.
// The caller has joined old workers and reserved candidate binding resources.
// localNonce was generated independently here, before reading the peer's offer.
func PrepareRecoveryTransaction(binding SessionBinding, gate *GenerationGate, token uint64, a Attachment, localNonce [16]byte, local, remote RecoveryView, export Exporter) (tx *RecoveryTransaction, err error) {
	if gate == nil || (gate.Phase() != AttachmentSuspended && gate.Phase() != AttachmentNegotiating) || binding.Role > 1 || binding.SessionID == ([32]byte{}) || binding.Capability == ([32]byte{}) || binding.IdentityDigest == ([32]byte{}) || a.SessionID != binding.SessionID || a.IdentityDigest != binding.IdentityDigest || localNonce == ([16]byte{}) || export == nil {
		return nil, ErrBounds
	}
	own, other := a.CoordinatorNonce, a.ParticipantNonce
	if binding.Role == 1 {
		own, other = other, own
	}
	if own != localNonce || other == ([16]byte{}) {
		return nil, ErrBounds
	}
	context, err := a.Context()
	if err != nil {
		return nil, err
	}
	if err = gate.Begin(token, a.Generation, Mode(a.Mode), context); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			gate.Lost(token)
		}
	}()
	coordinator, participant := local, remote
	if binding.Role == 1 {
		coordinator, participant = remote, local
	}
	digest, err := RecoveryDigest(coordinator, participant)
	if err != nil || digest != a.SnapshotDigest {
		return nil, ErrBounds
	}
	if err = ValidateRecovery(local, remote, binding.Role); err != nil {
		return nil, err
	}
	material, err := export(AttachmentExporterLabel, context[:], 32)
	if err != nil {
		return nil, err
	}
	if len(material) != 32 {
		return nil, ErrBounds
	}
	tx = &RecoveryTransaction{gate: gate, token: token, generation: a.Generation, capability: binding.Capability, context: context, role: binding.Role}
	copy(tx.exporter[:], material)
	tx.proof, err = AttachmentProof(tx.capability, tx.exporter, context, tx.role)
	if err != nil {
		return nil, err
	}
	return tx, nil
}
func (t *RecoveryTransaction) Proof() [32]byte { return t.proof }
func (t *RecoveryTransaction) AcceptPeerProof(proof []byte) error {
	if t == nil || t.gate == nil {
		return ErrBounds
	}
	if !VerifyAttachmentProof(t.capability, t.exporter, t.context, 1-t.role, proof) {
		return ErrAttachmentProof
	}
	return t.gate.Ready(t.token, t.generation)
}

// Commit invokes the flow owner once, before any COMMIT bytes. The owner
// reconciles current local state against the authenticated peer view.
// Any apply failure is terminal: a partially changed session cannot roll back.
// Receipt/loss/retransmission of protocol messages never applies it twice.
func (t *RecoveryTransaction) Commit(apply func() error) error {
	if t == nil || t.gate == nil {
		return ErrBounds
	}
	if !t.applied {
		if t.gate.Phase() != AttachmentReady || !t.gate.matches(t.token, t.generation) || apply == nil {
			return ErrBounds
		}
		if err := apply(); err != nil {
			t.gate.Close()
			return err
		}
		t.applied = true
	}
	return t.gate.Commit(t.token, t.generation)
}

// Activate follows ordered COMMIT_ACK ownership/receipt. Underlay suspension of
// an established promoted connection never creates a new transaction.
func (t *RecoveryTransaction) Activate() error {
	if t == nil || t.gate == nil || !t.applied {
		return ErrBounds
	}
	return t.gate.Activate(t.token, t.generation)
}

// Clear drops retained authority once no protocol retransmission can reference
// this attempt. It does not change the enclosing session's generation gate.
func (t *RecoveryTransaction) Clear() {
	if t != nil {
		*t = RecoveryTransaction{}
	}
}
