package directstream

// ExchangeConfig is supplied by the authorized session owner after freezing old
// workers. Binding must come from locally authenticated identities, not an offer.
// For initial participant admission, the owner accepts the random ID/capability
// only after enforcing principal, role, BLE eligibility and session uniqueness.
// Local is an immutable snapshot. Apply reconciles the current flow state;
// a partial failure is terminal. This driver performs no blocking transport I/O.
type ExchangeConfig struct {
	Binding          SessionBinding
	Gate             *GenerationGate
	Token            uint64
	Nonce            [16]byte
	Local            RecoveryView
	MaxRemoteRecords uint16
	Export           Exporter
	Apply            func() error
}

type exchangePhase byte

const (
	exchangeWaitView exchangePhase = iota
	exchangeWaitRecords
	exchangeWaitReady
	exchangeWaitCommit
	exchangeWaitAck
	exchangeActive
	exchangeFailed
)

// AttachmentExchange owns a single candidate connection attempt. Next remains
// unchanged under writer backpressure. Accepted means the ordered writer owns a
// copy, not that TLS has completed writing. Call it before admitting peer input.
// A transport failure calls Lost; never transplant this driver to a new TLS
// connection. A new connection needs a fresh token, alias, nonce and generation.
type AttachmentExchange struct {
	cfg         ExchangeConfig
	attachment  Attachment
	phase       exchangePhase
	remote      RecoveryView
	remoteCount int
	tx          *RecoveryTransaction
	out         []ControlRecord
}

func newExchange(cfg ExchangeConfig, a Attachment) (*AttachmentExchange, error) {
	if cfg.Gate == nil || cfg.Token == 0 || cfg.Nonce == ([16]byte{}) || cfg.Export == nil || cfg.Apply == nil || cfg.Binding.Role > 1 || !cfg.Local.valid() || cfg.Binding.SessionID == ([32]byte{}) || cfg.Binding.Capability == ([32]byte{}) || cfg.Binding.IdentityDigest == ([32]byte{}) {
		return nil, ErrBounds
	}
	if err := cfg.Gate.Reserve(cfg.Token, a.Generation, Mode(a.Mode)); err != nil {
		return nil, err
	}
	cfg.Local.Records = append([]RecoveryRecord(nil), cfg.Local.Records...)
	return &AttachmentExchange{cfg: cfg, attachment: a}, nil
}
func initialView(v RecoveryView) bool {
	return len(v.Records) == 0 && v.Namespaces[0].HighWater == 0 && v.Namespaces[1].HighWater == 0 && v.Namespaces[0].Admitted == 0 && v.Namespaces[1].Admitted == 0
}
func NewCoordinatorExchange(cfg ExchangeConfig, alias uint32, generation uint64, mode Mode, create bool) (*AttachmentExchange, error) {
	if cfg.Binding.Role != 0 || (create && !initialView(cfg.Local)) {
		return nil, ErrBounds
	}
	offer := ControlRecord{Kind: ControlOffer, AttachmentID: alias, Generation: generation, Mode: mode, Create: create, SessionID: cfg.Binding.SessionID, Nonce: cfg.Nonce, Namespaces: cfg.Local.Namespaces, Count: uint16(len(cfg.Local.Records))}
	if create {
		offer.Capability = cfg.Binding.Capability
	}
	if _, err := offer.Size(); err != nil {
		return nil, err
	}
	a := Attachment{AttachmentID: alias, Generation: generation, Mode: byte(mode), SessionID: cfg.Binding.SessionID, IdentityDigest: cfg.Binding.IdentityDigest, CoordinatorNonce: cfg.Nonce}
	e, err := newExchange(cfg, a)
	if err != nil {
		return nil, err
	}
	e.phase = exchangeWaitView
	e.queueView(offer)
	return e, nil
}
func NewParticipantExchange(cfg ExchangeConfig, offer ControlRecord) (*AttachmentExchange, error) {
	if cfg.Binding.Role != 1 || (offer.Create && (!initialView(cfg.Local) || offer.Count != 0 || !initialView(RecoveryView{Namespaces: offer.Namespaces}))) || offer.Kind != ControlOffer || offer.SessionID != cfg.Binding.SessionID || (offer.Create && offer.Capability != cfg.Binding.Capability) {
		return nil, ErrBounds
	}
	if _, err := offer.Size(); err != nil {
		return nil, err
	}
	a := Attachment{AttachmentID: offer.AttachmentID, Generation: offer.Generation, Mode: byte(offer.Mode), SessionID: cfg.Binding.SessionID, IdentityDigest: cfg.Binding.IdentityDigest, CoordinatorNonce: offer.Nonce, ParticipantNonce: cfg.Nonce}
	e, err := newExchange(cfg, a)
	if err != nil {
		return nil, err
	}
	if err = e.startView(offer); err != nil {
		e.Lost()
		return nil, err
	}
	return e, nil
}
func (e *AttachmentExchange) queueView(header ControlRecord) {
	e.out = append(e.out, header)
	for start := 0; start < len(e.cfg.Local.Records); start += MaxRecoveryBatch {
		end := min(start+MaxRecoveryBatch, len(e.cfg.Local.Records))
		e.out = append(e.out, ControlRecord{Kind: ControlRecords, AttachmentID: e.attachment.AttachmentID, Start: uint16(start), Records: e.cfg.Local.Records[start:end]})
	}
}
func (e *AttachmentExchange) startView(header ControlRecord) error {
	if header.Count > e.cfg.MaxRemoteRecords {
		return ErrBounds
	}
	// Validate checkpoint shape before reserving storage; record-level admission
	// bounds are checked when the complete transcript arrives.
	empty := RecoveryView{Namespaces: header.Namespaces}
	if !empty.valid() {
		return ErrBounds
	}
	e.remote = empty
	e.remote.Records = make([]RecoveryRecord, 0, int(header.Count))
	e.remoteCount = int(header.Count)
	e.phase = exchangeWaitRecords
	if e.remoteCount == 0 {
		return e.viewComplete()
	}
	return nil
}
func (e *AttachmentExchange) viewComplete() error {
	if !e.remote.valid() {
		return ErrBounds
	}
	if e.cfg.Binding.Role == 0 {
		e.phase = exchangeWaitReady
		return nil
	}
	if err := e.prepare(); err != nil {
		return err
	}
	e.queueView(ControlRecord{Kind: ControlView, AttachmentID: e.attachment.AttachmentID, Nonce: e.cfg.Nonce, Namespaces: e.cfg.Local.Namespaces, Count: uint16(len(e.cfg.Local.Records))})
	e.out = append(e.out, e.proofRecord(ControlReady))
	e.phase = exchangeWaitCommit
	return nil
}
func (e *AttachmentExchange) prepare() error {
	c, p := e.cfg.Local, e.remote
	if e.cfg.Binding.Role == 1 {
		c, p = p, c
	}
	digest, err := RecoveryDigest(c, p)
	if err != nil {
		return err
	}
	e.attachment.SnapshotDigest = digest
	e.tx, err = PrepareRecoveryTransaction(e.cfg.Binding, e.cfg.Gate, e.cfg.Token, e.attachment, e.cfg.Nonce, e.cfg.Local, e.remote, e.cfg.Export)
	return err
}
func (e *AttachmentExchange) proofRecord(kind byte) ControlRecord {
	return ControlRecord{Kind: kind, AttachmentID: e.attachment.AttachmentID, Context: e.tx.context, Proof: e.tx.Proof()}
}
func (e *AttachmentExchange) Next() (ControlRecord, bool) {
	if e == nil || len(e.out) == 0 || e.phase == exchangeFailed {
		return ControlRecord{}, false
	}
	return e.out[0], true
}
func (e *AttachmentExchange) Accepted() error {
	if e == nil || len(e.out) == 0 || e.phase == exchangeFailed {
		return ErrBounds
	}
	kind := e.out[0].Kind
	e.out[0] = ControlRecord{}
	e.out = e.out[1:]
	if kind == ControlCommitAck {
		if err := e.tx.Activate(); err != nil {
			e.Lost()
			return err
		}
		e.phase = exchangeActive
	}
	return nil
}

// Receive must consume a complete decoded control record from this candidate's
// single ordered reader. Unknown, out-of-phase and alias-mismatched messages
// terminate this attempt. Duplicate READY/COMMIT/ACK replay recorded responses,
// never reapply snapshots after traffic has advanced.
func (e *AttachmentExchange) Receive(c ControlRecord) (err error) {
	if e == nil || e.phase == exchangeFailed {
		return ErrBounds
	}
	defer func() {
		if err != nil {
			e.Lost()
		}
	}()
	if _, err = c.Size(); err != nil {
		return err
	}
	if c.AttachmentID != e.attachment.AttachmentID {
		return ErrBounds
	}
	switch c.Kind {
	case ControlView:
		if e.cfg.Binding.Role != 0 || e.phase != exchangeWaitView || len(e.out) != 0 {
			return ErrBounds
		}
		e.attachment.ParticipantNonce = c.Nonce
		return e.startView(c)
	case ControlRecords:
		if e.phase != exchangeWaitRecords || int(c.Start) != len(e.remote.Records) || len(e.remote.Records)+len(c.Records) > e.remoteCount {
			return ErrBounds
		}
		if len(e.remote.Records) > 0 && c.Records[0].ID <= e.remote.Records[len(e.remote.Records)-1].ID {
			return ErrBounds
		}
		e.remote.Records = append(e.remote.Records, c.Records...)
		if len(e.remote.Records) == e.remoteCount {
			return e.viewComplete()
		}
		return nil
	case ControlReady:
		if e.cfg.Binding.Role != 0 || (e.phase != exchangeWaitReady && e.phase != exchangeWaitAck && e.phase != exchangeActive) {
			return ErrBounds
		}
		if e.tx == nil {
			if err = e.prepare(); err != nil {
				return err
			}
		}
		if c.Context != e.tx.context {
			return ErrBounds
		}
		if err = e.tx.AcceptPeerProof(c.Proof[:]); err != nil {
			return err
		}
		if err = e.tx.Commit(e.cfg.Apply); err != nil {
			return err
		}
		if len(e.out) == 0 {
			e.out = append(e.out, e.proofRecord(ControlCommit))
		}
		if e.phase != exchangeActive {
			e.phase = exchangeWaitAck
		}
		return nil
	case ControlCommit:
		if len(e.out) != 0 && (len(e.out) != 1 || e.out[0].Kind != ControlCommitAck) {
			return ErrBounds
		}
		if e.cfg.Binding.Role != 1 || (e.phase != exchangeWaitCommit && e.phase != exchangeActive) || e.tx == nil || c.Context != e.tx.context {
			return ErrBounds
		}
		if err = e.tx.AcceptPeerProof(c.Proof[:]); err != nil {
			return err
		}
		if err = e.tx.Commit(e.cfg.Apply); err != nil {
			return err
		}
		if len(e.out) == 0 {
			e.out = append(e.out, ControlRecord{Kind: ControlCommitAck, AttachmentID: e.attachment.AttachmentID, Context: e.tx.context})
		} else if len(e.out) != 1 || e.out[0].Kind != ControlCommitAck {
			return ErrBounds
		}
		return nil
	case ControlCommitAck:
		if e.cfg.Binding.Role != 0 || (e.phase != exchangeWaitAck && e.phase != exchangeActive) || e.tx == nil || len(e.out) != 0 || c.Context != e.tx.context {
			return ErrBounds
		}
		if err = e.tx.Activate(); err != nil {
			return err
		}
		e.phase = exchangeActive
		return nil
	default:
		return ErrBounds
	}
}
func (e *AttachmentExchange) Active() bool { return e != nil && e.phase == exchangeActive }
func (e *AttachmentExchange) Lost() {
	if e == nil {
		return
	}
	e.cfg.Gate.Lost(e.cfg.Token)
	e.phase = exchangeFailed
	for i := range e.out {
		e.out[i] = ControlRecord{}
	}
	e.out = nil
	if e.tx != nil {
		e.tx.Clear()
	}
	e.cfg.Binding.Capability = [32]byte{}
}

// PeerView returns an owned copy of the complete, validated peer snapshot for
// the session owner's commit callback. It is not authority before peer proof.
func (e *AttachmentExchange) PeerView() (RecoveryView, error) {
	if e == nil || e.tx == nil || e.phase == exchangeFailed {
		return RecoveryView{}, ErrBounds
	}
	v := e.remote
	v.Records = append([]RecoveryRecord(nil), v.Records...)
	return v, nil
}

// CommittedAttachment returns the locally validated transcript only after both
// sides' proofs and the final commit acknowledgement have activated it.
func (e *AttachmentExchange) CommittedAttachment() (Attachment, error) {
	if !e.Active() {
		return Attachment{}, ErrBounds
	}
	return e.attachment, nil
}
