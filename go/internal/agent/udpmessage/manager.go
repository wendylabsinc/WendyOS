package udpmessage

import (
	"bytes"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const Lifetime = 30 * time.Second
const (
	FlowRXMessages  = 4
	FlowTXMessages  = 8
	FlowBytes       = 256 << 10
	PeerRXMessages  = 16
	PeerTXMessages  = 32
	PeerBytes       = 1 << 20
	NodeRXMessages  = 128
	NodeTXMessages  = 256
	NodeBytes       = 8 << 20
	NodeFlows       = 256
	PeerFlows       = 32
	NodeSocketBytes = 128 << 20
	PeerSocketBytes = 16 << 20
	FlowSocketBytes = 2 << 20
)

// Guard must run one bounded, nonblocking admission while the same exact app
// incarnation remains authorized. The caller never nests registry guards.
type Guard func(func() error) error

type usage struct{ messages, bytes int }
type peerState struct {
	peer        meshid.Device
	flows       []*Endpoint
	next        int
	rx, tx      usage
	socketBytes int
}
type reservation struct {
	endpoint *Endpoint
	bytes    int
	tx       bool
	released bool
}
type assembly struct {
	id                             uint64
	total, stride, count, received int
	born                           time.Time
	data                           []byte
	present                        [MaxFragments]bool
	reservation                    *reservation
}
type outgoing struct {
	id            uint64
	payload       []byte
	stride, index int
	born, retry   time.Time
	backoff       time.Duration
	reservation   *reservation
}

// Stats uses fixed fields and no attacker-controlled metric labels. Byte counts
// cover owned messages; QUIC/kernel queues retain their separate transport caps.
type Stats struct {
	RXBytes, TXBytes, RXMessages, TXMessages, Flows                  int
	SocketBytes                                                      int
	RXHighWater, TXHighWater                                         int
	RXMessageHighWater, TXMessageHighWater                           int
	FlowHighWater, SocketHighWater                                   int
	NegotiatedChunkMin, NegotiatedChunkMax, SendChunkMin             int
	NegotiatedChunkSeenMin, NegotiatedChunkSeenMax, SendChunkSeenMin int

	TransportAdmissions     uint64
	LocalDeliveryAdmissions uint64
	ReceivedChunks          uint64
	ReassembledMessages     uint64
	CanceledMessages        uint64
	BudgetRejects           uint64
	ExpiredMessages         uint64
	EvictedMessages         uint64
	MalformedChunks         uint64
	DuplicateChunks         uint64
	LateChunks              uint64
	ConflictingMessages     uint64
	TransportQueueRefusals  uint64
	SizeDrops               uint64
	GuardRejects            uint64
	TransportFailures       uint64
}

type Manager struct {
	mu               sync.Mutex
	peers            map[meshid.Device]*peerState
	order            []*peerState
	next             int
	rx, tx           usage
	stats            Stats
	closed           bool
	stop, wake, done chan struct{}
	closeDone        chan struct{}
	once             sync.Once
	now              func() time.Time
}

func NewManager() *Manager {
	return &Manager{peers: make(map[meshid.Device]*peerState), stop: make(chan struct{}), wake: make(chan struct{}, 1), done: make(chan struct{}), closeDone: make(chan struct{}), now: time.Now}
}
func (m *Manager) start() { m.once.Do(func() { go m.run() }) }
func (m *Manager) notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		<-m.closeDone
		return
	}
	m.closed = true
	close(m.stop)
	var endpoints []*Endpoint
	for _, p := range m.order {
		endpoints = append(endpoints, p.flows...)
	}
	m.mu.Unlock()
	for _, e := range endpoints {
		e.Close()
	}
	m.start()
	<-m.done
	close(m.closeDone)
}
func (m *Manager) Snapshot() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.stats
	s.RXBytes = m.rx.bytes
	s.TXBytes = m.tx.bytes
	s.RXMessages = m.rx.messages
	s.TXMessages = m.tx.messages
	for _, p := range m.order {
		for _, e := range p.flows {
			if e.try != nil && !e.local {
				if s.NegotiatedChunkMin == 0 || e.receiveMaximum < s.NegotiatedChunkMin {
					s.NegotiatedChunkMin = e.receiveMaximum
				}
				s.NegotiatedChunkMax = max(s.NegotiatedChunkMax, e.receiveMaximum)
				if s.SendChunkMin == 0 || e.maximum < s.SendChunkMin {
					s.SendChunkMin = e.maximum
				}
			}
		}
	}
	return s
}

// Endpoint belongs to exactly one authenticated UDP flow. The guard protects
// receive admission and every later transport admission, not only Send().
type Endpoint struct {
	manager        *Manager
	peer           *peerState
	guard          Guard
	closed         atomic.Bool
	sendMu         sync.Mutex
	deliveryMu     sync.Mutex
	try            func([]byte) error
	local          bool
	maximum        int
	receiveMaximum int
	socketBytes    int
	queue          []*outgoing
	nextID, high   uint64
	states         [WindowSize]uint8
	assemblies     map[uint64]*assembly
	completed      map[*Message]struct{}
	rx, tx         usage
	done           chan struct{}
	retired        chan struct{}
}

func (m *Manager) Open(peer meshid.Device, guard Guard) (*Endpoint, error) {
	if !peer.IsValid() || guard == nil {
		return nil, ErrMalformed
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	p := m.peers[peer]
	if m.stats.Flows >= NodeFlows || p != nil && len(p.flows) >= PeerFlows {
		return nil, ErrCapacity
	}
	if p == nil {
		p = &peerState{peer: peer}
		m.peers[peer] = p
		m.order = append(m.order, p)
	}
	e := &Endpoint{manager: m, peer: p, guard: guard, maximum: MaxChunkSize, receiveMaximum: MaxChunkSize, nextID: 1, assemblies: make(map[uint64]*assembly), completed: make(map[*Message]struct{}), done: make(chan struct{}), retired: make(chan struct{})}
	p.flows = append(p.flows, e)
	m.stats.Flows++
	m.stats.FlowHighWater = max(m.stats.FlowHighWater, m.stats.Flows)
	m.start()
	return e, nil
}
func (e *Endpoint) Done() <-chan struct{} { return e.done }
func (e *Endpoint) Manager() *Manager     { return e.manager }
func (e *Endpoint) SetLocalTransport(maximum int, guard Guard, send func([]byte) error) error {
	if guard == nil {
		return ErrMalformed
	}
	return e.attach(maximum, true, guard, send)
}
func (e *Endpoint) SetTransport(maximum int, local bool, send func([]byte) error) error {
	return e.attach(maximum, local, nil, send)
}
func (e *Endpoint) attach(maximum int, local bool, guard Guard, send func([]byte) error) error {
	if maximum < MinChunkSize || maximum > MaxChunkSize || send == nil {
		return ErrMalformed
	}
	e.sendMu.Lock()
	defer e.sendMu.Unlock()
	e.deliveryMu.Lock()
	defer e.deliveryMu.Unlock()
	m := e.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.closed.Load() {
		return ErrClosed
	}
	if e.try != nil {
		return errors.New("UDP message transport already attached")
	}
	if guard != nil {
		e.guard = guard
	}
	e.maximum = maximum
	e.receiveMaximum = maximum
	e.local = local
	e.try = send
	if !local {
		if m.stats.NegotiatedChunkSeenMin == 0 || maximum < m.stats.NegotiatedChunkSeenMin {
			m.stats.NegotiatedChunkSeenMin = maximum
		}
		m.stats.NegotiatedChunkSeenMax = max(m.stats.NegotiatedChunkSeenMax, maximum)
		if m.stats.SendChunkSeenMin == 0 || maximum < m.stats.SendChunkSeenMin {
			m.stats.SendChunkSeenMin = maximum
		}
	}
	m.notify()
	return nil
}

// ReserveSocketBytes accounts actual kernel-reported buffers for this flow's
// one local connected socket. It runs before transport attachment or delivery.
func (e *Endpoint) ReserveSocketBytes(n int) error {
	m := e.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.closed.Load() {
		return ErrClosed
	}
	if n <= 0 || n > FlowSocketBytes || e.peer.socketBytes+n-e.socketBytes > PeerSocketBytes || m.stats.SocketBytes+n-e.socketBytes > NodeSocketBytes {
		return ErrCapacity
	}
	delta := n - e.socketBytes
	e.socketBytes = n
	e.peer.socketBytes += delta
	m.stats.SocketBytes += delta
	m.stats.SocketHighWater = max(m.stats.SocketHighWater, m.stats.SocketBytes)
	return nil
}

// WithGuard serializes final receive/delivery actions with Close. Ordered
// transport writes have a separate lock so a full write window cannot stop
// the application from draining replies. The callback must not acquire
// another registry authorization lock.
func (e *Endpoint) WithGuard(action func() error) error {
	e.deliveryMu.Lock()
	defer e.deliveryMu.Unlock()
	if e.closed.Load() {
		return ErrClosed
	}
	m := e.manager
	m.mu.Lock()
	guard := e.guard
	m.mu.Unlock()
	called := false
	err := guard(func() error {
		called = true
		if e.closed.Load() {
			return ErrClosed
		}
		return action()
	})
	if err != nil && !called {
		m.RecordDenied()
	}
	return err
}

// Deliver admits one complete message to a local kernel socket. Success is
// local kernel admission, never proof that the application consumed it.
func (e *Endpoint) Deliver(send func() error) error {
	err := e.WithGuard(send)
	m := e.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.stats.LocalDeliveryAdmissions++
	}
	return err
}

func (e *Endpoint) Close() {
	if !e.closed.CompareAndSwap(false, true) {
		<-e.retired
		return
	}
	close(e.done)
	defer close(e.retired)
	// Join both admission directions before releasing their reservations.
	e.sendMu.Lock()
	e.sendMu.Unlock()
	e.deliveryMu.Lock()
	e.deliveryMu.Unlock()
	m := e.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats.CanceledMessages += uint64(len(e.queue) + len(e.assemblies) + len(e.completed))
	for _, msg := range e.queue {
		m.releaseLocked(msg.reservation)
	}
	e.queue = nil
	for _, a := range e.assemblies {
		m.releaseLocked(a.reservation)
	}
	clear(e.assemblies)
	for msg := range e.completed {
		m.releaseLocked(msg.reservation)
	}
	clear(e.completed)
	p := e.peer
	for i, item := range p.flows {
		if item == e {
			copy(p.flows[i:], p.flows[i+1:])
			p.flows[len(p.flows)-1] = nil
			p.flows = p.flows[:len(p.flows)-1]
			break
		}
	}
	m.stats.Flows--
	m.stats.SocketBytes -= e.socketBytes
	p.socketBytes -= e.socketBytes
	e.socketBytes = 0
	if len(p.flows) == 0 {
		delete(m.peers, p.peer)
		for i, item := range m.order {
			if item == p {
				copy(m.order[i:], m.order[i+1:])
				m.order[len(m.order)-1] = nil
				m.order = m.order[:len(m.order)-1]
				break
			}
		}
	}
}
func (m *Manager) reserveLocked(e *Endpoint, n int, tx bool) (*reservation, error) {
	charge := max(n, 256)
	flow, peer, node := &e.rx, &e.peer.rx, &m.rx
	fc, pc, nc := FlowRXMessages, PeerRXMessages, NodeRXMessages
	if tx {
		flow, peer, node = &e.tx, &e.peer.tx, &m.tx
		fc, pc, nc = FlowTXMessages, PeerTXMessages, NodeTXMessages
	}
	if flow.messages >= fc || flow.bytes+charge > FlowBytes || peer.messages >= pc || peer.bytes+charge > PeerBytes || node.messages >= nc || node.bytes+charge > NodeBytes {
		return nil, ErrCapacity
	}
	for _, u := range []*usage{flow, peer, node} {
		u.messages++
		u.bytes += charge
	}
	m.stats.RXHighWater = max(m.stats.RXHighWater, m.rx.bytes)
	m.stats.TXHighWater = max(m.stats.TXHighWater, m.tx.bytes)
	m.stats.RXMessageHighWater = max(m.stats.RXMessageHighWater, m.rx.messages)
	m.stats.TXMessageHighWater = max(m.stats.TXMessageHighWater, m.tx.messages)
	return &reservation{endpoint: e, bytes: charge, tx: tx}, nil
}
func (m *Manager) releaseLocked(r *reservation) {
	if r == nil || r.released {
		return
	}
	r.released = true
	e := r.endpoint
	values := []*usage{&e.rx, &e.peer.rx, &m.rx}
	if r.tx {
		values = []*usage{&e.tx, &e.peer.tx, &m.tx}
	}
	for _, u := range values {
		u.messages--
		u.bytes -= r.bytes
	}
}

// Send reserves/copies a complete message before publishing it. Success means
// local queue admission, never network or app delivery. Empty UDP is valid.
func (e *Endpoint) Send(payload []byte) error {
	if len(payload) > MaxMessageSize {
		return ErrMalformed
	}
	if e.closed.Load() {
		return ErrClosed
	}
	m := e.manager
	m.mu.Lock()
	if e.closed.Load() {
		m.mu.Unlock()
		return ErrClosed
	}
	r, err := m.reserveLocked(e, len(payload), true)
	if err != nil {
		m.stats.BudgetRejects++
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	owned := append([]byte(nil), payload...)
	m.mu.Lock()
	guard := e.guard
	m.mu.Unlock()
	err = guard(func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		if e.closed.Load() {
			return ErrClosed
		}
		if e.nextID == 0 || e.nextID == math.MaxUint64 {
			return errors.New("UDP message ID exhausted")
		}
		e.queue = append(e.queue, &outgoing{id: e.nextID, payload: owned, born: m.now(), reservation: r})
		e.nextID++
		return nil
	})
	if err != nil {
		m.mu.Lock()
		m.stats.GuardRejects++
		m.releaseLocked(r)
		m.mu.Unlock()
		return err
	}
	m.notify()
	return nil
}
func (e *Endpoint) expireLocked(now time.Time) {
	m := e.manager
	for id, a := range e.assemblies {
		if !now.Before(a.born.Add(Lifetime)) {
			delete(e.assemblies, id)
			e.states[id%WindowSize] = 3
			m.releaseLocked(a.reservation)
			m.stats.ExpiredMessages++
		}
	}
	for i := 0; i < len(e.queue); {
		q := e.queue[i]
		if !now.Before(q.born.Add(Lifetime)) {
			copy(e.queue[i:], e.queue[i+1:])
			e.queue[len(e.queue)-1] = nil
			e.queue = e.queue[:len(e.queue)-1]
			m.releaseLocked(q.reservation)
			m.stats.ExpiredMessages++
			continue
		}
		i++
	}
}
func (e *Endpoint) advanceLocked(id uint64) {
	if id <= e.high {
		return
	}
	delta := id - e.high
	if delta >= WindowSize {
		clear(e.states[:])
	} else {
		for n := uint64(1); n <= delta; n++ {
			e.states[(e.high+n)%WindowSize] = 0
		}
	}
	e.high = id
	floor := uint64(1)
	if id >= WindowSize {
		floor = id - WindowSize + 1
	}
	for prior, a := range e.assemblies {
		if prior < floor {
			delete(e.assemblies, prior)
			e.manager.releaseLocked(a.reservation)
			e.manager.stats.EvictedMessages++
		}
	}
}
func (e *Endpoint) evictLocked() bool {
	var oldest *assembly
	var owner *Endpoint
	best := -1
	m := e.manager
	for _, p := range m.order {
		for _, flow := range p.flows {
			rank := 0
			switch {
			case flow == e:
				rank = 3
			case p == e.peer:
				rank = 2
			case m.rx.bytes >= NodeBytes-MaxMessageSize || m.rx.messages >= NodeRXMessages:
				if p.rx.bytes > MaxMessageSize {
					rank = 1
				}
			default:
				continue
			}
			for _, a := range flow.assemblies {
				if owner == nil || rank > best || rank == best && a.born.Before(oldest.born) {
					oldest, owner, best = a, flow, rank
				}
			}
		}
	}
	if oldest == nil {
		return false
	}
	delete(owner.assemblies, oldest.id)
	owner.states[oldest.id%WindowSize] = 3
	m.releaseLocked(oldest.reservation)
	m.stats.EvictedMessages++
	return true
}

// Message retains the RX reservation through a final local delivery. Release
// must follow every successful owned receive, including failed delivery.
// Completed messages cannot be evicted to admit more incomplete traffic.
type Message struct {
	Payload     []byte
	reservation *reservation
}

func (msg *Message) Release() {
	if msg == nil || msg.reservation == nil {
		return
	}
	r := msg.reservation
	m := r.endpoint.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(r.endpoint.completed, msg)
	m.releaseLocked(r)
}

// RetainReceived reserves and copies one complete local read before delivery.
// The full-size socket read buffer has its separate bounded scratch allowance.
func (e *Endpoint) RetainReceived(payload []byte) (*Message, error) {
	if len(payload) > MaxMessageSize {
		return nil, ErrMalformed
	}
	m := e.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.closed.Load() {
		return nil, ErrClosed
	}
	r, err := m.reserveLocked(e, len(payload), false)
	if err != nil {
		m.stats.BudgetRejects++
		return nil, err
	}
	msg := &Message{Payload: append([]byte(nil), payload...), reservation: r}
	e.completed[msg] = struct{}{}
	return msg, nil
}

// ReceiveFrame never returns partial data and retains completed bytes until
// Release or endpoint retirement. The caller must recheck final delivery tokens.
func (e *Endpoint) ReceiveFrame(frame []byte) (message *Message, err error) {
	m := e.manager
	m.mu.Lock()
	maximum, guard := e.receiveMaximum, e.guard
	m.mu.Unlock()
	f, err := decode(frame, maximum)
	if err != nil {
		m.mu.Lock()
		m.stats.MalformedChunks++
		m.mu.Unlock()
		return nil, nil
	}
	err = guard(func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		if e.closed.Load() {
			return ErrClosed
		}
		now := m.now()
		e.expireLocked(now)
		m.stats.ReceivedChunks++
		if e.high >= WindowSize && f.id <= e.high-WindowSize {
			m.stats.LateChunks++
			return nil
		}
		e.advanceLocked(f.id)
		state := e.states[f.id%WindowSize]
		if state >= 2 {
			m.stats.LateChunks++
			return nil
		}
		a := e.assemblies[f.id]
		if a != nil && (a.total != f.total || a.stride != f.stride || a.count != f.count) {
			delete(e.assemblies, f.id)
			m.releaseLocked(a.reservation)
			e.states[f.id%WindowSize] = 3
			m.stats.ConflictingMessages++
			return nil
		}
		if a == nil {
			var r *reservation
			var reserveErr error
			for attempt := 0; attempt <= 4; attempt++ {
				r, reserveErr = m.reserveLocked(e, f.total, false)
				if reserveErr == nil {
					break
				}
				if attempt == 4 || !e.evictLocked() {
					break
				}
			}
			if reserveErr != nil {
				e.states[f.id%WindowSize] = 3
				m.stats.BudgetRejects++
				return nil
			}
			a = &assembly{id: f.id, total: f.total, stride: f.stride, count: f.count, born: now, data: make([]byte, f.total), reservation: r}
			e.assemblies[f.id] = a
			e.states[f.id%WindowSize] = 1
		}
		offset := f.index * f.stride
		if a.present[f.index] {
			if !bytes.Equal(a.data[offset:offset+len(f.data)], f.data) {
				delete(e.assemblies, f.id)
				m.releaseLocked(a.reservation)
				e.states[f.id%WindowSize] = 3
				m.stats.ConflictingMessages++
			} else {
				m.stats.DuplicateChunks++
			}
			return nil
		}
		copy(a.data[offset:], f.data)
		a.present[f.index] = true
		a.received++
		if a.received == a.count {
			message = &Message{Payload: a.data, reservation: a.reservation}
			e.completed[message] = struct{}{}
			delete(e.assemblies, f.id)
			e.states[f.id%WindowSize] = 2
			m.stats.ReassembledMessages++
		}
		return nil
	})
	if err != nil {
		m.RecordDenied()
	}
	return message, err
}

// RecordDenied counts a rejected authorization without attacker-controlled labels.
func (m *Manager) RecordDenied() { m.mu.Lock(); m.stats.GuardRejects++; m.mu.Unlock() }
