package directstream

import "math"

type Mode uint8

const (
	DirectBLE Mode = iota
	Promoted
)

type AttachmentPhase uint8

const (
	AttachmentActive AttachmentPhase = iota
	AttachmentSuspended
	AttachmentNegotiating
	AttachmentPreparing
	AttachmentReady
	AttachmentCommitting
	AttachmentClosed
)

// GenerationGate is the actor-owned attachment fence, not a transport or flow
// owner. A token identifies one authenticated TLS connection locally; a new
// connection, including resumed TLS, MUST receive a fresh token. All old I/O
// workers must be joined/fenced before Frozen returns a snapshot to Begin.
// Proof, flow reconciliation and authorization are prerequisites to Ready.
type GenerationGate struct {
	initialized                     bool
	phase                           AttachmentPhase
	mode                            Mode
	promotion                       bool
	generation, reserved, token     uint64
	pendingGeneration, pendingToken uint64
	pendingMode                     Mode
	context                         [32]byte
}

func NewGenerationGate(generation, token uint64, mode Mode) (*GenerationGate, error) {
	if generation == 0 || token == 0 || mode > Promoted {
		return nil, ErrBounds
	}
	return &GenerationGate{initialized: true, phase: AttachmentActive, mode: mode, promotion: mode == Promoted, generation: generation, reserved: generation, token: token}, nil
}

// NewPendingGenerationGate creates a session that has never admitted traffic.
// Its first reserved generation is 1; an uninitialized zero value stays closed.
func NewPendingGenerationGate() *GenerationGate {
	return &GenerationGate{initialized: true, phase: AttachmentSuspended}
}

func (g *GenerationGate) Phase() AttachmentPhase { return g.phase }
func (g *GenerationGate) NextGeneration() (uint64, bool) {
	if !g.initialized || g.phase == AttachmentClosed || g.reserved == math.MaxUint64 {
		return 0, false
	}
	return g.reserved + 1, true
}
func (g *GenerationGate) Allows(token, generation uint64) bool {
	return g.initialized && token != 0 && generation != 0 && g.phase == AttachmentActive && token == g.token && generation == g.generation
}

// Frozen is called AFTER joining old ingress and writer workers. It is also
// used for a deliberate faster-link promotion while the old carrier is live.
// The caller must stop DATA dispatch before beginning the join, not after it.
func (g *GenerationGate) Frozen(token uint64) error {
	if !g.initialized || token == 0 || g.phase != AttachmentActive || token != g.token || g.mode == Promoted {
		return ErrBounds
	}
	g.phase = AttachmentSuspended
	g.token = 0
	return nil
}

// Reserve burns the generation before an OFFER can escape to the peer. The
// context is not known until both independently chosen nonces and views arrive.
func (g *GenerationGate) Reserve(token, generation uint64, mode Mode) error {
	if !g.initialized || token == 0 || mode > Promoted {
		return ErrBounds
	}
	if g.phase == AttachmentNegotiating && g.pendingToken == token && g.pendingGeneration == generation && g.pendingMode == mode {
		return nil
	}
	if g.phase != AttachmentSuspended || generation <= g.reserved || (g.promotion && mode != Promoted) {
		return ErrBounds
	}
	g.reserved = generation
	g.pendingGeneration = generation
	g.pendingToken = token
	g.pendingMode = mode
	g.promotion = g.promotion || mode == Promoted
	g.phase = AttachmentNegotiating
	return nil
}

// Begin reserves a generation permanently, even if its handshake later fails.
// Retries on the same connection use the exact same tuple. A new connection
// uses a higher generation and fresh reconciled snapshots; a stale COMMIT may
// never reset offsets after application bytes have advanced.
func (g *GenerationGate) Begin(token, generation uint64, mode Mode, context [32]byte) error {
	if !g.initialized {
		return ErrBounds
	}
	if token != 0 && token == g.pendingToken && generation == g.pendingGeneration && mode == g.pendingMode && context == g.context && ((g.phase >= AttachmentPreparing && g.phase <= AttachmentCommitting) || (g.phase == AttachmentActive && g.generation == generation)) {
		return nil
	}
	if err := g.Reserve(token, generation, mode); err != nil {
		return err
	}
	g.context = context
	g.phase = AttachmentPreparing
	return nil
}
func (g *GenerationGate) matches(token, generation uint64) bool {
	return g.initialized && token != 0 && token == g.pendingToken && generation == g.pendingGeneration
}
func (g *GenerationGate) Ready(token, generation uint64) error {
	if !g.matches(token, generation) {
		return ErrBounds
	}
	if g.phase == AttachmentPreparing {
		g.phase = AttachmentReady
		return nil
	}
	if g.phase == AttachmentReady || g.phase == AttachmentCommitting || (g.phase == AttachmentActive && g.generation == generation) {
		return nil
	}
	return ErrBounds
}

// Commit is recorded before attempting the coordinator's COMMIT write, or
// after the participant validates that message. No rollback follows an
// ambiguous COMMIT write. All per-flow binding resources must already exist.
func (g *GenerationGate) Commit(token, generation uint64) error {
	if !g.matches(token, generation) {
		return ErrBounds
	}
	if g.phase == AttachmentReady {
		g.phase = AttachmentCommitting
		return nil
	}
	if g.phase == AttachmentCommitting || (g.phase == AttachmentActive && g.generation == generation) {
		return nil
	}
	return ErrBounds
}

// Activate follows receipt of COMMIT_ACK (coordinator), or successful ownership
// of that ACK by the ordered writer (participant). Across independent upper
// streams, readers must hold/backpressure early data until local activation.
func (g *GenerationGate) Activate(token, generation uint64) error {
	if !g.matches(token, generation) {
		return ErrBounds
	}
	if g.phase == AttachmentActive && g.generation == generation {
		return nil
	}
	if g.phase != AttachmentCommitting {
		return ErrBounds
	}
	g.phase = AttachmentActive
	g.generation = generation
	g.token = token
	g.mode = g.pendingMode
	return nil
}

// Lost means the authenticated connection itself died, not an underlay outage
// while upper QUIC/TCP remains alive. After promotion COMMIT may have enabled
// bare application bytes, so loss is terminal; custom recovery must not be
// invented after dropping its retention. Before COMMIT, retry remains possible
// but promotion intent stays sticky. Direct BLE always retains logical state.
func (g *GenerationGate) Lost(token uint64) bool {
	active := g.phase == AttachmentActive && token != 0 && token == g.token
	pending := g.phase >= AttachmentNegotiating && g.phase <= AttachmentCommitting && token != 0 && token == g.pendingToken
	if !active && !pending {
		return false
	}
	terminal := active && g.mode == Promoted || pending && g.pendingMode == Promoted && g.phase == AttachmentCommitting
	if terminal {
		g.phase = AttachmentClosed
	} else {
		g.phase = AttachmentSuspended
	}
	g.token = 0
	g.pendingToken = 0
	return true
}

// Close fences every callback on timeout, credential/trust invalidation, owner
// shutdown or terminal flow failure. Only a new session may leave this state.
func (g *GenerationGate) Close() {
	g.phase = AttachmentClosed
	g.token = 0
	g.pendingToken = 0
}
