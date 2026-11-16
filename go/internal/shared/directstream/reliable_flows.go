package directstream

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

var ErrOpenPending = errors.New("direct OPEN is pending")

const (
	ResultDenied uint16 = 1 + iota
	ResultCapacity
	ResultCancelled
	ResultIO
	ResultUnsupported
)

// OpenRequest is immutable for a flow's lifetime. Native service authorization
// remains the caller's responsibility; this owner never dials a destination.
type OpenRequest struct {
	Service            byte
	Port, DatagramSize uint16
}

func (r OpenRequest) digest() ([32]byte, error) {
	f := FlowFrame{Kind: FrameOpen, AttachmentID: 1, FlowID: 1, Service: r.Service, Port: r.Port, DatagramSize: r.DatagramSize}
	if _, err := f.Size(); err != nil {
		return [32]byte{}, err
	}
	var b [5]byte
	b[0] = r.Service
	binary.BigEndian.PutUint16(b[1:], r.Port)
	binary.BigEndian.PutUint16(b[3:], r.DatagramSize)
	h := sha256.New()
	h.Write([]byte("wendy/direct/open/1\x00"))
	h.Write(b[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

type IncomingOpen struct {
	ID      uint32
	Request OpenRequest
}
type reliableFlow struct {
	open                                                   OpenEntry
	packet                                                 *DatagramConn
	promotionDone                                          <-chan struct{}
	request                                                OpenRequest
	lane                                                   *ReliableLane
	reset                                                  bool
	resetCode                                              uint16
	openPending, resultPending, resetPending, closePending bool
}

// ReliableFlows combines bounded OPEN admission, retained app sockets and the
// direct record scheduler. Its methods belong to one session actor. Only the
// returned RetainedConn may be used concurrently by application/relay workers.
// Backend dial completions must be posted to that actor by never-reused ID.
// UDP uses a separately budgeted datagram owner when explicitly enabled by the
// session. Default instances refuse UDP until their backend supports it.
type ReliableFlows struct {
	datagramLimit, peerDatagramLimit        uint16
	datagramLimitPending, peerDatagramKnown bool
	datagrams                               *DatagramBudget
	datagramGeneration                      uint64
	datagramWire                            [MaxFlowFrame]byte
	closed                                  bool
	promoted                                bool
	upperControl                            bool
	idleSent, peerIdle                      bool
	order                                   []uint32
	role                                    byte
	registries                              [2]*OpenNamespace
	flows                                   map[uint32]*reliableFlow
	budget                                  *ConnBudget
	window, initial                         uint64
	nextID                                  uint32
	alias                                   uint32
	creditPending                           bool
	pending                                 FlowFrame
	pendingLane                             *ReliableLane
	scratch                                 [MaxDataPayload]byte
	cursor                                  uint32
	// Recovery can require a close confirmation for an already retired ID, or
	// the proved identity/cancellation of an OPEN not yet replayed.
	retiredACK    map[uint32]bool
	expectedOpens map[uint32]RecoveryRecord
}

func NewReliableFlows(role byte, slots, txBytes, rxBytes int, window, initial uint64) (*ReliableFlows, error) {
	if role > 1 || slots < 1 || slots > 32767 || window == 0 || initial == 0 || initial > window || window%ChunkSize != 0 || initial%ChunkSize != 0 {
		return nil, ErrBounds
	}
	b, err := NewConnBudget(txBytes, rxBytes)
	if err != nil {
		return nil, err
	}
	m := &ReliableFlows{order: make([]uint32, 0, 2*slots), role: role, budget: b, window: window, initial: initial, flows: make(map[uint32]*reliableFlow), nextID: 2 - uint32(role), creditPending: true, retiredACK: make(map[uint32]bool), expectedOpens: make(map[uint32]RecoveryRecord)}
	for i := range m.registries {
		m.registries[i], err = NewOpenNamespace(slots, i == 0)
		if err != nil {
			return nil, err
		}
	}
	if err = m.registries[1-role].Grant(uint64(slots)); err != nil {
		return nil, err
	}
	return m, nil
}
func (m *ReliableFlows) Changed() <-chan struct{} { return m.budget.Changed() }
func (m *ReliableFlows) Bind(alias uint32) error {
	if m.promoted {
		return ErrPromoted
	}
	if m.closed || alias == 0 || m.alias != 0 {
		return ErrBounds
	}
	if m.datagramGeneration == math.MaxUint64 {
		return ErrBounds
	}
	m.datagramGeneration++
	for _, f := range m.flows {
		if f.packet != nil && f.packet.terminalError() == nil {
			if err := f.packet.Bind(m.datagramGeneration, alias); err != nil {
				return err
			}
		}
	}
	m.alias = alias
	m.datagramLimitPending = true
	m.peerDatagramKnown = false
	m.peerDatagramLimit = 0
	m.creditPending = true
	return nil
}

// Suspend follows joining the old carrier reader/writer. It preserves app
// sockets and slabs; pending output is reconciled, never blindly retransmitted.
func (m *ReliableFlows) Suspend() {
	m.alias = 0
	m.pending = FlowFrame{}
	m.pendingLane = nil
	for _, f := range m.flows {
		if f.packet != nil {
			f.packet.Suspend()
		}
	}
}
func (m *ReliableFlows) ids() []uint32 { return m.order }
func (m *ReliableFlows) put(id uint32, f *reliableFlow) {
	if _, exists := m.flows[id]; !exists {
		at := sort.Search(len(m.order), func(i int) bool { return m.order[i] >= id })
		m.order = append(m.order, 0)
		copy(m.order[at+1:], m.order[at:])
		m.order[at] = id
	}
	m.flows[id] = f
}

// The flow map is the sole record of OPEN admission, result and closure.
func (m *ReliableFlows) entry(id uint32) (*reliableFlow, OpenEntry, error) {
	f := m.flows[id]
	if f == nil {
		return nil, OpenEntry{}, ErrRetired
	}
	return f, f.open, nil
}
func (m *ReliableFlows) openEntry(id uint32) (OpenEntry, error) {
	_, e, err := m.entry(id)
	return e, err
}
func (m *ReliableFlows) startOpen(id uint32, digest [32]byte, f *reliableFlow) (bool, error) {
	if old := m.flows[id]; old != nil {
		if old.open.Digest != digest {
			return false, ErrBounds
		}
		return false, nil
	}
	if err := m.registries[id&1].Admit(id); err != nil {
		return false, err
	}
	if f == nil {
		f = &reliableFlow{}
	}
	f.open = OpenEntry{Digest: digest}
	m.put(id, f)
	return true, nil
}
func (m *ReliableFlows) completeOpen(id uint32, result uint16) error {
	f := m.flows[id]
	if f == nil {
		return ErrRetired
	}
	if f.open.Complete && f.open.Result != result {
		return ErrBounds
	}
	f.open.Complete = true
	f.open.Result = result
	return nil
}
func (m *ReliableFlows) closeOpen(id uint32, remote bool) (bool, error) {
	f := m.flows[id]
	if f == nil {
		if id != 0 && id <= m.registries[id&1].HighWater() {
			return true, nil
		}
		return false, ErrBounds
	}
	if !f.open.Complete {
		return false, ErrBounds
	}
	if remote {
		f.open.RemoteClosed = true
	} else {
		f.open.LocalClosed = true
	}
	return f.open.LocalClosed && f.open.RemoteClosed, nil
}

// Called with the shared budget lock held. Recycled chunks must remain
// available to restore each admitted receiver's minimum progress reservation.
// This is local allocation policy; advertised credit is never revoked.
func (m *ReliableFlows) canReserveReceive(owner *RetainedConn, bytes uint64) bool {
	free := uint64(m.budget.rx.Available())
	for _, flow := range m.flows {
		if flow.reset || flow.lane == nil {
			continue
		}
		c := flow.lane.Conn
		if c == owner || c.terminal != nil || c.promoted != nil {
			continue
		}
		r := &c.receiver
		if r.finished && r.Buffer.End() == r.final {
			continue
		}
		held := r.Buffer.Limit() - r.Buffer.Start()/ChunkSize*ChunkSize
		if held < m.initial {
			deficit := m.initial - held
			if deficit > free {
				return false
			}
			free -= deficit
		}
	}
	return bytes <= free
}

func (m *ReliableFlows) makeFlow(id uint32, r OpenRequest, now int64) (*reliableFlow, error) {
	if r.Service == ServiceUDP {
		if m.datagrams == nil || (m.datagramLimit == 0 || r.DatagramSize > m.datagramLimit) {
			return nil, ErrBounds
		}
		packet, err := m.datagrams.NewConn(id, int(r.DatagramSize), nil, nil)
		if err != nil {
			return nil, err
		}
		if err = packet.Bind(m.datagramGeneration, m.alias); err != nil {
			packet.Abort(err)
			return nil, err
		}
		return &reliableFlow{request: r, packet: packet}, nil
	}
	c, err := m.budget.newConn(m.window, m.initial, nil, nil, m.initial, func() bool { return m.canReserveReceive(nil, m.initial) })
	if err != nil {
		return nil, err
	}
	l, err := NewReliableLane(id, c, now)
	if err != nil {
		c.Abort(err)
		return nil, err
	}
	return &reliableFlow{request: r, lane: l}, nil
}

// Begin does not expose the socket before OPEN_RESULT, so pending requests can
// be recovered without application bytes or a second destination dial.
func (m *ReliableFlows) Begin(r OpenRequest, now int64) (uint32, error) {
	if m.promoted {
		return 0, ErrPromoted
	}
	if m.alias == 0 || m.nextID == 0 || now < 0 {
		return 0, ErrBounds
	}
	if r.Service == ServiceUDP && m.datagrams == nil {
		return 0, ErrBounds
	}
	if r.Service == ServiceUDP {
		if !m.peerDatagramKnown {
			return 0, ErrCapacity
		}
		if m.peerDatagramLimit == 0 || m.datagramLimit == 0 {
			return 0, ErrBounds
		}
		if r.DatagramSize != 0 {
			r.DatagramSize = min(r.DatagramSize, min(m.datagramLimit, m.peerDatagramLimit))
		}
	}
	digest, err := r.digest()
	if err != nil {
		return 0, err
	}
	id := m.nextID
	f, err := m.makeFlow(id, r, now)
	if err != nil {
		return 0, err
	}
	if _, err = m.startOpen(id, digest, f); err != nil {
		if f.lane != nil {
			f.lane.Conn.Abort(err)
		}
		if f.packet != nil {
			f.packet.Abort(err)
		}
		return 0, err
	}
	if id > math.MaxUint32-2 {
		m.nextID = 0
	} else {
		m.nextID += 2
	}
	f.openPending = true
	return id, nil
}
func (m *ReliableFlows) Connection(id uint32) (*RetainedConn, error) {
	f, e, err := m.entry(id)
	if err != nil {
		return nil, err
	}
	if !e.Complete {
		return nil, ErrOpenPending
	}
	if e.Result != 0 || f.reset || f.lane == nil {
		return nil, fmt.Errorf("direct flow refused: result=%d reset=%d", e.Result, f.resetCode)
	}
	return f.lane.Conn, nil
}

// CompleteIncoming records one authorized dial result. A delayed completion
// after cancellation/retirement is rejected; its caller must close that backend.
func (m *ReliableFlows) CompleteIncoming(id uint32, code uint16) (*RetainedConn, error) {
	f, e, err := m.entry(id)
	if err != nil || byte(id&1) == m.role {
		return nil, ErrBounds
	}
	if f.reset {
		return nil, ErrRetired
	}
	if e.Complete && e.Result != code {
		return nil, ErrBounds
	}
	if code == 0 && f.lane == nil && f.packet == nil {
		return nil, ErrCapacity
	}
	if err = m.completeOpen(id, code); err != nil {
		return nil, err
	}
	f.resultPending = true
	if code != 0 {
		if f.lane != nil {
			f.lane.Conn.Abort(errors.New("direct OPEN rejected"))
		}
		if f.packet != nil {
			f.packet.Abort(errors.New("direct OPEN rejected"))
		}
		f.closePending = true
		return nil, nil
	}
	if f.packet != nil {
		return nil, nil
	}
	return f.lane.Conn, nil
}
func (m *ReliableFlows) resetFlow(id uint32, f *reliableFlow, code uint16, send bool) {
	if !f.reset {
		f.reset = true
		f.resetCode = code
		if f.packet != nil {
			f.packet.Abort(fmt.Errorf("direct RESET %d", code))
		}
		if f.lane != nil {
			f.lane.Conn.Abort(fmt.Errorf("direct RESET %d", code))
		}
	}
	f.resetPending = f.resetPending || send
	f.closePending = true
	// Cancellation is terminal even while the backend dial is pending. Receiver
	// owns the result and returns a stable rejection without waiting for that dial.
	e, _ := m.openEntry(id)
	if !e.Complete && byte(id&1) != m.role {
		_ = m.completeOpen(id, ResultCancelled)
		f.resultPending = true
	}
}
func (m *ReliableFlows) appClosed(id uint32, f *reliableFlow, err error) bool {
	if err == nil || f.lane == nil || f.lane.Conn.terminalError() == nil {
		return false
	}
	m.resetFlow(id, f, ResultCancelled, true)
	return true
}
func (m *ReliableFlows) Reset(id uint32, code uint16) error {
	f, _, err := m.entry(id)
	if err != nil {
		return err
	}
	m.resetFlow(id, f, code, true)
	return nil
}
func (m *ReliableFlows) retire(id uint32, f *reliableFlow) {
	if f.packet != nil {
		f.packet.Abort(ErrRetired)
	}
	if f.promotionDone != nil {
		// UDP was closed above and has no reliable replay/FIN lane to drain.
		if f.lane != nil && !f.lane.Conn.retirePromoted() {
			f.lane.Conn.Abort(ErrRetired)
		}
		<-f.promotionDone
	}
	if f.lane != nil && !f.reset {
		_ = f.lane.Conn.WithFlow(func(s *Sender, r *Receiver) (bool, error) {
			f.lane.Conn.removeSendShare()
			s.Buffer.Close()
			r.Buffer.Close()
			s.finished = true
			r.finished = true
			s.final = 0
			r.final = 0
			return true, nil
		})
	}
	m.registries[id&1].free++
	delete(m.flows, id)
	at := sort.Search(len(m.order), func(i int) bool { return m.order[i] >= id })
	if at < len(m.order) && m.order[at] == id {
		m.order = append(m.order[:at], m.order[at+1:]...)
	}
	delete(m.expectedOpens, id)
	// Released metadata slots may be promised again, independently of bytes.
	r := m.registries[1-m.role]
	if err := r.Grant(r.Admitted() + uint64(r.Available())); err == nil {
		m.creditPending = !m.upperControl
	}
}
func (m *ReliableFlows) Receive(f FlowFrame, now int64) (*IncomingOpen, error) {
	if m.alias == 0 || f.AttachmentID != m.alias || now < 0 {
		return nil, ErrBounds
	}
	if _, err := f.Size(); err != nil {
		return nil, err
	}
	if m.upperControl && (f.Kind == FrameData || f.Kind == FrameState || f.Kind == FrameFIN || f.Kind == FrameDatagram) {
		return nil, ErrPromoted
	}
	switch f.Kind {
	case FrameEpochIdle:
		if !m.upperControl {
			return nil, ErrBounds
		}
		m.peerIdle = true
		return nil, nil
	case FrameDatagramLimit:
		if m.upperControl || m.peerDatagramKnown && m.peerDatagramLimit != f.DatagramSize {
			return nil, ErrBounds
		}
		m.peerDatagramLimit = f.DatagramSize
		m.peerDatagramKnown = true
		return nil, nil
	case FrameOpenCredit:
		return nil, m.registries[m.role].GrantPeer(f.Value)
	case FrameOpen:
		if byte(f.FlowID&1) == m.role {
			return nil, ErrBounds
		}
		request := OpenRequest{f.Service, f.Port, f.DatagramSize}
		digest, err := request.digest()
		if err != nil {
			return nil, err
		}
		// Recovery carries only the OPEN digest until an unseen request is
		// replayed. Now its service is known, enforce UDP's zero byte state
		// before allocating an association or invoking the backend.
		if replay, ok := m.expectedOpens[f.FlowID]; ok {
			if replay.OpenDigest != digest || (request.Service == ServiceUDP && replay.Flow != (FlowSnapshot{})) {
				return nil, ErrBounds
			}
		}
		if m.upperControl {
			_, known := m.flows[f.FlowID]
			_, expected := m.expectedOpens[f.FlowID]
			if !known && !expected {
				return nil, ErrBounds
			}
		}
		fresh, err := m.startOpen(f.FlowID, digest, nil)
		if err != nil {
			return nil, err
		}
		if !fresh {
			old, e, err := m.entry(f.FlowID)
			if err != nil {
				return nil, err
			}
			old.resultPending = old.resultPending || e.Complete
			return nil, nil
		}
		old := m.flows[f.FlowID]
		old.request = request
		if replay, ok := m.expectedOpens[f.FlowID]; ok {
			delete(m.expectedOpens, f.FlowID)
			if replay.Reset {
				m.resetFlow(f.FlowID, old, replay.ResetCode, false)
				return nil, nil
			}
		}
		code := ResultCapacity
		if request.Service == ServiceUDP && m.datagrams == nil {
			code = ResultUnsupported
		} else {
			created, err := m.makeFlow(f.FlowID, request, now)
			if err == nil {
				created.open = old.open
				m.put(f.FlowID, created)
				return &IncomingOpen{f.FlowID, request}, nil
			}
		}
		_, err = m.CompleteIncoming(f.FlowID, code)
		return nil, err
	case FrameState:
		for _, state := range f.States[:f.StateCount] {
			flow, e, err := m.entry(state.FlowID)
			if err != nil {
				return nil, err
			}
			if flow.reset {
				continue
			}
			if !e.Complete || e.Result != 0 || flow.lane == nil {
				return nil, ErrBounds
			}
			if err = flow.lane.State(state); err != nil && !m.appClosed(state.FlowID, flow, err) {
				return nil, err
			}
		}
		return nil, nil
	case FrameCloseACK:
		flow, _, err := m.entry(f.FlowID)
		if err != nil {
			_, err = m.closeOpen(f.FlowID, true)
			return nil, err
		}
		retired, err := m.closeOpen(f.FlowID, true)
		if retired {
			m.retire(f.FlowID, flow)
		}
		return nil, err
	}
	flow, e, err := m.entry(f.FlowID)
	if err != nil {
		return nil, err
	}
	switch f.Kind {
	case FrameOpenResult:
		if byte(f.FlowID&1) != m.role {
			return nil, ErrBounds
		}
		if err = m.completeOpen(f.FlowID, f.Code); err != nil {
			return nil, err
		}
		if f.Code != 0 {
			if flow.packet != nil {
				flow.packet.Abort(fmt.Errorf("direct OPEN rejected %d", f.Code))
			}
			flow.closePending = true
			if flow.lane != nil {
				flow.lane.Conn.Abort(fmt.Errorf("direct OPEN rejected %d", f.Code))
			}
		}
		return nil, nil
	case FrameReset:
		m.resetFlow(f.FlowID, flow, f.Code, false)
		return nil, nil
	case FrameDatagram:
		if flow.reset {
			return nil, nil
		}
		if !e.Complete || e.Result != 0 || flow.packet == nil {
			return nil, ErrBounds
		}
		err := flow.packet.Receive(m.datagramGeneration, f)
		if err != nil && flow.packet.terminalError() != nil {
			m.resetFlow(f.FlowID, flow, ResultCancelled, true)
			return nil, nil
		}
		return nil, err
	case FrameData, FrameFIN:
		if flow.reset {
			return nil, nil
		}
		if !e.Complete || e.Result != 0 || flow.lane == nil {
			return nil, ErrBounds
		}
		err = flow.lane.Receive(f, now)
		if m.appClosed(f.FlowID, flow, err) {
			return nil, nil
		}
		return nil, err
	default:
		return nil, ErrBounds
	}
}

// Prepare retains one immutable output under backpressure; Accepted is called
// immediately after the writer owns a COPY, before processing inbound records.
func (m *ReliableFlows) Prepare(now int64) (FlowFrame, bool, error) {
	if m.alias == 0 || now < 0 {
		return FlowFrame{}, false, ErrBounds
	}
	if m.pending.Kind != 0 {
		return m.pending, true, nil
	}
	set := func(f FlowFrame) (FlowFrame, bool, error) { m.pending = f; return f, true, nil }
	if m.upperControl && !m.idleSent && m.Quiescent() {
		return set(FlowFrame{Kind: FrameEpochIdle, AttachmentID: m.alias})
	}
	if m.datagramLimitPending {
		return set(FlowFrame{Kind: FrameDatagramLimit, AttachmentID: m.alias, DatagramSize: m.datagramLimit})
	}
	if m.creditPending {
		return set(FlowFrame{Kind: FrameOpenCredit, AttachmentID: m.alias, Value: m.registries[1-m.role].GrantLimit()})
	}
	for id := range m.retiredACK {
		return set(FlowFrame{Kind: FrameCloseACK, AttachmentID: m.alias, FlowID: id})
	}
	// OPEN admission is monotonic within our namespace. A fair DATA cursor
	// must never let a newer OPEN overtake an older pending request.
	for _, id := range m.ids() {
		flow := m.flows[id]
		if flow.openPending {
			return set(FlowFrame{Kind: FrameOpen, AttachmentID: m.alias, FlowID: id, Service: flow.request.Service, Port: flow.request.Port, DatagramSize: flow.request.DatagramSize})
		}
	}
	ids := m.ids()
	start := sort.Search(len(ids), func(i int) bool { return ids[i] > m.cursor })
	for i := 0; i < len(ids); i++ {
		id := ids[(start+i)%len(ids)]
		flow, e, _ := m.entry(id)
		base := FlowFrame{AttachmentID: m.alias, FlowID: id}
		if flow.packet != nil && !flow.reset && flow.packet.terminalError() != nil {
			m.resetFlow(id, flow, ResultCancelled, true)
		}
		if m.upperControl && !flow.reset && flow.lane != nil && flow.lane.Conn.terminalError() != nil {
			m.resetFlow(id, flow, ResultCancelled, true)
		}
		if m.upperControl && !e.LocalClosed && !flow.reset && flow.lane != nil && flow.lane.Conn.promotedFinished() {
			flow.closePending = true
		}
		switch {
		case flow.openPending:
			base.Kind = FrameOpen
			base.Service = flow.request.Service
			base.Port = flow.request.Port
			base.DatagramSize = flow.request.DatagramSize
		case flow.resultPending:
			base.Kind = FrameOpenResult
			base.Code = e.Result
		case flow.resetPending:
			base.Kind = FrameReset
			base.Code = flow.resetCode
		case flow.closePending && e.Complete:
			base.Kind = FrameCloseACK
		default:
			if m.upperControl {
				continue
			}
			if flow.packet != nil && e.Complete && e.Result == 0 && !flow.reset && !e.LocalClosed {
				n, _, _, _, err := flow.packet.Prepare(m.datagramWire[:])
				if err != nil {
					m.resetFlow(id, flow, ResultCancelled, true)
					continue
				}
				if n == 0 {
					continue
				}
				record, err := DecodeFlowFrame(m.datagramWire[:n])
				if err != nil {
					return FlowFrame{}, false, err
				}
				return set(record)
			}
			if !e.Complete || e.Result != 0 || flow.reset || flow.lane == nil || e.LocalClosed {
				continue
			}
			snapshot, err := flow.lane.Snapshot()
			if err != nil {
				m.resetFlow(id, flow, ResultCancelled, true)
				continue
			}
			if snapshot.SendFinished && snapshot.ReceiveFinished && snapshot.Acked == snapshot.SendFinal && snapshot.Consumed == snapshot.ReceiveFinal && flow.lane.finACK && !flow.lane.ack.pending {
				flow.closePending = true
				continue
			}
			// Admission stays small; refill consuming flows toward the configured
			// working window in slab quanta. Shared allocation may refuse
			// growth without blocking ACKs or scheduling other admitted flows.
			if (!snapshot.ReceiveFinished || snapshot.Received < snapshot.ReceiveFinal) && snapshot.Received > 0 && snapshot.Limit-snapshot.Received <= m.window/2 {
				if snapshot.Consumed > math.MaxUint64-m.window {
					return FlowFrame{}, false, ErrBounds
				}
				aim := snapshot.Consumed + m.window
				if snapshot.ReceiveFinished {
					aim = min(aim, snapshot.ReceiveFinal)
				}
				if aim > snapshot.Limit {
					limit := snapshot.Limit + min(aim-snapshot.Limit, ChunkSize)
					_ = flow.lane.grant(limit, now, func(r *Receiver) bool {
						if limit > math.MaxUint64-(ChunkSize-1) {
							return false
						}
						rounded := (limit + ChunkSize - 1) / ChunkSize * ChunkSize
						return m.canReserveReceive(flow.lane.Conn, rounded-r.Buffer.Limit())
					})
				}
			}
			record, ready, err := flow.lane.Prepare(m.alias, now, m.scratch[:])
			if err != nil {
				if m.appClosed(id, flow, err) {
					continue
				}
				return FlowFrame{}, false, err
			}
			if !ready {
				continue
			}
			m.pendingLane = flow.lane
			return set(record)
		}
		return set(base)
	}
	return FlowFrame{}, false, nil
}
func (m *ReliableFlows) Accepted() error {
	p := m.pending
	if p.Kind == 0 {
		return ErrBounds
	}
	m.pending = FlowFrame{}
	if p.FlowID != 0 {
		m.cursor = p.FlowID
	}
	if m.pendingLane != nil {
		l := m.pendingLane
		m.cursor = l.ID
		m.pendingLane = nil
		return l.Accepted()
	}
	if p.Kind == FrameEpochIdle {
		m.idleSent = true
		return nil
	}
	if p.Kind == FrameDatagramLimit {
		m.datagramLimitPending = false
		return nil
	}
	if p.Kind == FrameOpenCredit {
		m.creditPending = m.registries[1-m.role].GrantLimit() != p.Value
		return nil
	}
	if m.retiredACK[p.FlowID] {
		delete(m.retiredACK, p.FlowID)
		return nil
	}
	f, _, err := m.entry(p.FlowID)
	if err != nil {
		// A close confirmation retained under backpressure may cross the
		// peer's final ACK, which retires the flow before this send completes.
		// Only close ACKs are idempotent here; never suppress stale DATA/RESET.
		if p.Kind == FrameCloseACK {
			_, err = m.closeOpen(p.FlowID, false)
		}
		return err
	}
	switch p.Kind {
	case FrameDatagram:
		if f.packet == nil {
			return ErrBounds
		}
		err := f.packet.Accepted(m.datagramGeneration, p.Value, p.FragmentOffset)
		if err != nil && f.packet.terminalError() != nil {
			m.resetFlow(p.FlowID, f, ResultCancelled, true)
			return nil
		}
		return err
	case FrameOpen:
		f.openPending = false
	case FrameOpenResult:
		f.resultPending = false
	case FrameReset:
		f.resetPending = false
	case FrameCloseACK:
		f.closePending = false
		retired, err := m.closeOpen(p.FlowID, false)
		if retired {
			m.retire(p.FlowID, f)
		}
		return err
	default:
		return ErrBounds
	}
	return nil
}
func (m *ReliableFlows) Snapshot() (RecoveryView, error) {
	if m.promoted {
		return RecoveryView{}, ErrPromoted
	}
	if m.closed {
		return RecoveryView{}, ErrBounds
	}
	v := RecoveryView{}
	for i, r := range m.registries {
		v.Namespaces[i] = NamespaceCheckpoint{r.HighWater(), r.Admitted(), r.GrantLimit()}
	}
	for _, id := range m.ids() {
		f, e, _ := m.entry(id)
		r := RecoveryRecord{ID: id, OpenDigest: e.Digest, Result: e.Result, OpenComplete: e.Complete, LocalClosed: e.LocalClosed, RemoteClosed: e.RemoteClosed, Reset: f.reset, ResetCode: f.resetCode}
		if !f.reset && (!e.Complete || e.Result == 0) && f.lane != nil {
			var err error
			r.Flow, err = f.lane.Snapshot()
			if err != nil {
				m.resetFlow(id, f, ResultCancelled, true)
				r.Reset = true
				r.ResetCode = f.resetCode
				r.Flow = FlowSnapshot{}
				e, _ = m.openEntry(id)
				r.OpenComplete = e.Complete
				r.Result = e.Result
			}
		}
		v.Records = append(v.Records, r)
	}
	if !v.valid() {
		return RecoveryView{}, ErrBounds
	}
	return v, nil
}

// ApplyRecovery is the transaction's commit callback, after every old I/O
// worker is joined. App reads/writes may have progressed locally; fresh local
// validation must still succeed before applying any peer-derived action.
func (m *ReliableFlows) ApplyRecovery(remote RecoveryView, now int64) error {
	if m.closed || m.alias != 0 || now < 0 || len(remote.Records) > m.registries[0].slots+m.registries[1].slots {
		return ErrBounds
	}
	// UDP retains its association but never reconciles reliable byte offsets.
	for _, record := range remote.Records {
		if f := m.flows[record.ID]; f != nil && f.packet != nil && record.Flow != (FlowSnapshot{}) {
			return ErrBounds
		}
	}
	local, err := m.Snapshot()
	if err != nil {
		return err
	}
	plan, err := PlanRecovery(local, remote, m.role)
	if err != nil {
		return err
	}
	records := make(map[uint32]RecoveryRecord, len(remote.Records))
	for _, r := range remote.Records {
		records[r.ID] = r
	}
	clear(m.expectedOpens)
	clear(m.retiredACK)
	for _, step := range plan {
		if step.Action == RecoveryReceiveOpen {
			m.expectedOpens[step.ID] = records[step.ID]
			continue
		}
		if step.Action == RecoveryConfirmRetired {
			m.retiredACK[step.ID] = true
			continue
		}
		f, e, err := m.entry(step.ID)
		if err != nil {
			return err
		}
		f.openPending = false
		f.resultPending = false
		f.resetPending = false
		f.closePending = e.LocalClosed || f.reset || (e.Complete && e.Result != 0)
		switch step.Action {
		case RecoveryRetire:
			retired, err := m.closeOpen(step.ID, true)
			if err != nil || !retired {
				return ErrBounds
			}
			m.retire(step.ID, f)
		case RecoveryReplayOpen:
			f.openPending = true
			f.resetPending = f.reset
		case RecoveryResume, RecoveryReset:
			if step.OpenComplete {
				if err = m.completeOpen(step.ID, step.Result); err != nil {
					return err
				}
			}
			if step.Action == RecoveryReset {
				m.resetFlow(step.ID, f, step.ResetCode, true)
				continue
			}
			if step.OpenComplete && step.Result != 0 {
				if f.packet != nil {
					f.packet.Abort(errors.New("recovered OPEN refusal"))
				}
				f.closePending = true
				if f.lane != nil {
					f.lane.Conn.Abort(errors.New("recovered OPEN refusal"))
				}
			} else if f.lane != nil {
				if err = f.lane.Reattach(records[step.ID].Flow, now); err != nil && !m.appClosed(step.ID, f, err) {
					return err
				}
			}
			if byte(step.ID&1) != m.role && step.OpenComplete && !records[step.ID].OpenComplete {
				f.resultPending = true
			}
		default:
			return ErrBounds
		}
	}
	if err = m.registries[m.role].GrantPeer(remote.Namespaces[m.role].Grant); err != nil {
		return err
	}
	m.creditPending = true
	return nil
}
func (m *ReliableFlows) Quiescent() bool {
	return len(m.flows) == 0 && len(m.expectedOpens) == 0 && len(m.retiredACK) == 0 && m.pending.Kind == 0
}

// RetirementReady proves both promoted owners have drained the immutable
// admitted flow set. The actor must also join pending backend/binding work and
// flush its copied control output before releasing the epoch. New normal upper
// connections are separate owners; their lifetime must gate fresh BLE admission.
func (m *ReliableFlows) RetirementReady() bool {
	return !m.closed && m.upperControl && m.idleSent && m.peerIdle && m.Quiescent()
}
func (m *ReliableFlows) Close(err error) {
	m.closed = true
	m.Suspend()
	for _, f := range m.flows {
		if f.packet != nil {
			f.packet.Abort(err)
		}
		if f.lane != nil {
			f.lane.Conn.Abort(err)
		}
	}
	for _, f := range m.flows {
		if f.promotionDone != nil {
			<-f.promotionDone
		}
	}
	clear(m.flows)
	m.order = nil
	clear(m.expectedOpens)
	clear(m.retiredACK)
}

// NextWake is the earliest monotonic microsecond deadline after Prepare has
// drained ready output. Unrelated activity must not postpone an old ACK timer.
func (m *ReliableFlows) NextWake(now int64) (int64, bool) {
	if m.closed || m.alias == 0 {
		return 0, false
	}
	if m.pending.Kind != 0 || m.creditPending || m.datagramLimitPending || len(m.retiredACK) != 0 {
		return now, true
	}
	var earliest int64
	have := false
	for id, f := range m.flows {
		e, _ := m.openEntry(id)
		if f.openPending || f.resultPending || f.resetPending || (f.closePending && e.Complete) {
			return now, true
		}
		if !m.upperControl && e.Complete && e.Result == 0 && !f.reset && !e.LocalClosed && f.lane != nil && f.lane.ack.pending {
			due := f.lane.ack.since + ACKDelayUS
			if !have || due < earliest {
				earliest = due
				have = true
			}
		}
	}
	return max(now, earliest), have
}
