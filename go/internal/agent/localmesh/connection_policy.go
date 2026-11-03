package localmesh

import (
	"context"
	"math/rand/v2"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const (
	BLEMaxOutgoing         = 2
	BLEMaxOutgoingAttempts = 1
	BLEMaxConnections      = 3
	NANMaxOutgoing         = 2
	NANMaxOutgoingAttempts = 2
	NANMaxConnections      = 4
	LANMaxOutgoing         = 2
	LANMaxOutgoingAttempts = 2
	LANMaxConnections      = 64
	FailedConnectPenalty   = 30 * time.Second
	IncomingAttemptIdle    = 5 * time.Second
	maxCandidateHistory    = 256
)

type ConnectionLimits struct {
	Outgoing, Attempts, Total int
}

// ConnectionPolicy belongs to one transport. Reservations include pending
// setup, so simultaneous discovery and inbound accepts cannot oversubscribe it.
// UUID ordering is used only to converge authenticated duplicate connections.
type ConnectionPolicy struct {
	mu          sync.Mutex
	self        meshid.Device
	preferLower bool
	limits      ConnectionLimits
	connections map[*ConnectionLease]struct{}
	now         func() time.Time
	snapshot    func() NodeSnapshot
	reclaiming  bool
}

func NewConnectionPolicy(self meshid.Device, preferLower bool, limits ConnectionLimits) *ConnectionPolicy {
	return &ConnectionPolicy{self: self, preferLower: preferLower, limits: limits, connections: make(map[*ConnectionLease]struct{}), now: time.Now}
}

type ConnectionLease struct {
	policy                   *ConnectionPolicy
	key                      string
	group                    string
	peer                     meshid.Device
	outgoing, ready, retired bool
	close                    func()
	onReady                  func()
	progress                 time.Time
	stopTracking             func()
	link                     babel.LinkID
	cost                     uint16
	retire                   func()
	reclaimed                bool
}

// SetSnapshot supplies local authenticated routing evidence for capacity
// reclamation. It never changes this provider's independent connection limits.
func (p *ConnectionPolicy) SetSnapshot(snapshot func() NodeSnapshot) {
	p.mu.Lock()
	p.snapshot = snapshot
	p.mu.Unlock()
}

type ConnectionCounts struct{ Outgoing, Attempts, Total, Ready int }

func (p *ConnectionPolicy) counts() (c ConnectionCounts) {
	for l := range p.connections {
		c.Total++
		if l.ready && !l.retired {
			c.Ready++
		}
		if l.outgoing {
			c.Outgoing++
			if !l.ready {
				c.Attempts++
			}
		}
	}
	return c
}

func (p *ConnectionPolicy) Counts() ConnectionCounts {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts()
}

// IncomingAvailable reports advisory discovery capacity, including pending leases.
// It never reserves a slot or changes established connection ownership.
func (p *ConnectionPolicy) IncomingAvailable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts().Total < p.limits.Total
}

func (p *ConnectionPolicy) preferred(l *ConnectionLease) bool {
	lower := meshid.Compare(p.self, l.peer) < 0
	return l.outgoing == (lower == p.preferLower)
}

func (p *ConnectionPolicy) Reserve(key string, peer meshid.Device, outgoing bool) *ConnectionLease {
	return p.ReserveGroup(key, peer, outgoing, "")
}

// Ethernet and infrastructure Wi-Fi share a budget, but their different costs
// must coexist during a cheaper-carrier replacement.
func (p *ConnectionPolicy) ReserveGroup(key string, peer meshid.Device, outgoing bool, group string) *ConnectionLease {
	return p.reserveGroup(key, peer, outgoing, group, false)
}

// ReservePreferred may release one slower redundant owned carrier when a
// runnable new direct peer needs this provider's capacity. The caller excludes
// candidates in setup retry/failure backoff before permitting reclamation.
// Closing never lends an extra slot: the connection's owner must Release first.
func (p *ConnectionPolicy) ReservePreferred(key string, peer meshid.Device, outgoing bool, group string, reclaim bool) *ConnectionLease {
	return p.reserveGroup(key, peer, outgoing, group, reclaim)
}

func (p *ConnectionPolicy) reserveGroup(key string, peer meshid.Device, outgoing bool, group string, reclaim bool) *ConnectionLease {
	p.mu.Lock()
	snapshot := p.snapshot
	p.mu.Unlock()
	var view NodeSnapshot
	if reclaim && snapshot != nil {
		view = snapshot()
	}
	p.mu.Lock()
	var retire func()
	defer func() {
		p.mu.Unlock()
		if retire != nil {
			retire()
		}
	}()
	l := &ConnectionLease{policy: p, key: key, group: group, peer: peer, outgoing: outgoing, progress: p.now()}
	for existing := range p.connections {
		if existing.retired {
			continue
		}
		if existing.outgoing == outgoing && existing.key == key {
			return nil
		}
		if existing.ready && existing.group == group && (existing.key == key || (peer != (meshid.Device{}) && existing.peer == peer)) {
			return nil
		}
		if outgoing && !existing.outgoing && !existing.ready && existing.group == group && p.now().Sub(existing.progress) < IncomingAttemptIdle &&
			(existing.key == key || (peer != (meshid.Device{}) && existing.peer == peer)) {
			return nil
		}
		if peer != (meshid.Device{}) && existing.peer == peer && existing.group == group {
			if existing.outgoing == outgoing {
				return nil
			}
		}
	}
	c := p.counts()
	if c.Total >= p.limits.Total || (outgoing && (c.Outgoing >= p.limits.Outgoing || c.Attempts >= p.limits.Attempts)) {
		if reclaim && snapshot != nil && !p.reclaiming && (!outgoing || c.Attempts < p.limits.Attempts) {
			retire = p.reclaim(view, peer, outgoing && c.Outgoing >= p.limits.Outgoing)
		}
		return nil
	}
	p.connections[l] = struct{}{}
	return l
}

// reclaim runs under the provider lock and rechecks exact authenticated lease
// ownership against one immutable routing snapshot. An indirect mesh route or
// discovery identity can never authorize dropping the last direct path.
func (p *ConnectionPolicy) reclaim(view NodeSnapshot, candidate meshid.Device, needOutgoing bool) func() {
	for _, link := range view.Links {
		if candidate != (meshid.Device{}) && link.Device == candidate && !link.Retiring {
			return nil // Upgrades and extra redundancy only use spare capacity.
		}
	}
	var victim *ConnectionLease
	for l := range p.connections {
		if !l.ready || l.retired || l.link == 0 || l.retire == nil || (needOutgoing && !l.outgoing) {
			continue
		}
		for _, link := range view.Links {
			if link.ID == l.link && link.Device == l.peer && link.Cost == l.cost && !link.Retiring && view.HasCheaperLink(l.peer, l.cost) {
				if victim == nil || l.cost > victim.cost || (l.cost == victim.cost && l.link < victim.link) {
					victim = l
				}
			}
		}
	}
	if victim == nil {
		return nil
	}
	victim.retired, victim.reclaimed, p.reclaiming = true, true, true
	return victim.retire
}

func (l *ConnectionLease) Release() {
	if l == nil {
		return
	}
	l.policy.mu.Lock()
	if l.stopTracking != nil {
		l.stopTracking()
	}
	delete(l.policy.connections, l)
	if l.reclaimed {
		l.policy.reclaiming = false
	}
	l.policy.mu.Unlock()
}

// Identify records an untrusted preface/discovery identity only for avoiding a
// crossed dial. It is bounded by handshake deadlines and the idle timeout;
// certificate verification and Ready remain the authentication boundary.
func (l *ConnectionLease) Identify(peer meshid.Device) {
	if l == nil {
		return
	}
	l.policy.mu.Lock()
	defer l.policy.mu.Unlock()
	if !l.ready && peer != l.policy.self && peer.Tenant() == l.policy.self.Tenant() {
		l.peer = peer
	}
}

// Progress is called on handshake I/O or a NAN negotiation milestone, never by
// polling or repeated advertisements. Silent incoming attempts stop suppressing
// outbound setup after IncomingAttemptIdle even while their deadline runs out.
func (l *ConnectionLease) Progress() {
	if l == nil {
		return
	}
	l.policy.mu.Lock()
	defer l.policy.mu.Unlock()
	if !l.ready && !l.retired {
		l.progress = l.policy.now()
	}
}

type incomingProgressConn struct {
	net.Conn
	lease    *ConnectionLease
	tracking atomic.Bool
}

func (l *ConnectionLease) TrackIncoming(conn net.Conn) net.Conn {
	if l == nil {
		return conn
	}
	c := &incomingProgressConn{Conn: conn, lease: l}
	c.tracking.Store(true)
	l.policy.mu.Lock()
	l.stopTracking = func() { c.tracking.Store(false) }
	l.policy.mu.Unlock()
	return c
}

func (c *incomingProgressConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && c.tracking.Load() {
		c.lease.Progress()
	}
	return n, err
}
func (c *incomingProgressConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 && c.tracking.Load() {
		c.lease.Progress()
	}
	return n, err
}
func (c *incomingProgressConn) NetConn() net.Conn { return c.Conn }

func (l *ConnectionLease) WasReady() bool {
	if l == nil {
		return false
	}
	l.policy.mu.Lock()
	defer l.policy.mu.Unlock()
	return l.ready
}

func (l *ConnectionLease) Retired() bool {
	if l == nil {
		return false
	}
	l.policy.mu.Lock()
	defer l.policy.mu.Unlock()
	return l.retired
}

func (l *ConnectionLease) ObserveReady(f func()) {
	if l == nil {
		return
	}
	l.policy.mu.Lock()
	l.onReady = f
	l.policy.mu.Unlock()
}

// Ready is called after certificate verification and the bidirectional mesh
// hello. Only the losing connection is closed; the peer's other sockets and
// Bluetooth ACL are never disconnected. Capacity is released by its owner.
func (l *ConnectionLease) Ready(peer meshid.Device, closeConnection func()) bool {
	if l == nil {
		return true
	}
	p := l.policy
	p.mu.Lock()
	if _, exists := p.connections[l]; !exists || l.retired || peer == p.self || peer.Tenant() != p.self.Tenant() || (l.peer != (meshid.Device{}) && l.peer != peer) {
		p.mu.Unlock()
		closeConnection()
		return false
	}
	l.peer, l.ready, l.close = peer, true, closeConnection
	if l.stopTracking != nil {
		l.stopTracking()
	}
	var closeOthers []func()
	for other := range p.connections {
		if other == l || !other.ready || other.retired || other.peer != peer || other.group != l.group {
			continue
		}
		// Retain the incumbent for same-direction duplicates. For crossed
		// connections both endpoints derive the same preferred initiator.
		if other.outgoing == l.outgoing || p.preferred(other) {
			l.retired = true
			closeOthers = append(closeOthers, l.close)
			break
		}
		other.retired = true
		closeOthers = append(closeOthers, other.close)
	}
	keep := !l.retired
	observer := l.onReady
	p.mu.Unlock()
	for _, closeOther := range closeOthers {
		closeOther()
	}
	if keep && observer != nil {
		observer()
	}
	return keep
}

type connectionLeaseKey struct{}

func WithConnectionLease(ctx context.Context, l *ConnectionLease) context.Context {
	return context.WithValue(ctx, connectionLeaseKey{}, l)
}

// BindConnectionLink connects a verified provider lease to its exact admitted
// Node incarnation. retire must only signal its existing lifetime owner.
func BindConnectionLink(ctx context.Context, id babel.LinkID, cost uint16, retire func()) {
	l, _ := ctx.Value(connectionLeaseKey{}).(*ConnectionLease)
	if l == nil {
		return
	}
	l.policy.mu.Lock()
	defer l.policy.mu.Unlock()
	if _, exists := l.policy.connections[l]; exists && l.ready && !l.retired {
		l.link, l.cost, l.retire = id, cost, retire
	}
}

// ConnectionReady is also usable by transport test nodes without kernel TUNs.
func ConnectionReady(ctx context.Context, peer meshid.Device, closeConnection func()) bool {
	l, _ := ctx.Value(connectionLeaseKey{}).(*ConnectionLease)
	return l.Ready(peer, closeConnection)
}

// CandidateQueue shuffles healthy candidates and puts failed candidates last,
// oldest failure first. A skipped candidate is never recorded as a failure.
// Its zero value is usable; history is bounded despite untrusted discovery.
type CandidateQueue struct {
	mu      sync.Mutex
	failed  map[string]time.Time
	shuffle func(int, func(int, int))
}

func (q *CandidateQueue) Failed(key string, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failed == nil {
		q.failed = make(map[string]time.Time)
	}
	if len(q.failed) >= maxCandidateHistory {
		oldest := ""
		var when time.Time
		for k, t := range q.failed {
			if oldest == "" || t.Before(when) {
				oldest, when = k, t
			}
		}
		delete(q.failed, oldest)
	}
	q.failed[key] = now
}

func (q *CandidateQueue) Succeeded(key string) {
	q.mu.Lock()
	delete(q.failed, key)
	q.mu.Unlock()
}

// ReclaimEligible keeps failed candidates from repeatedly evicting a standby
// during the existing failure penalty. Repeated advertisements do not reset it.
func (q *CandidateQueue) ReclaimEligible(key string, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	failed, ok := q.failed[key]
	return !ok || now.Sub(failed) >= FailedConnectPenalty
}

// OrderPreferred preserves the queue's retry ordering within each scheduling
// class: a new direct peer, a faster carrier, then slower spare redundancy.
func (q *CandidateQueue) OrderPreferred(keys []string, now time.Time, view NodeSnapshot, candidate func(string) (meshid.Device, uint16)) []string {
	out := q.Order(keys, now)
	rank := func(key string) int {
		if !q.ReclaimEligible(key, now) {
			return 3
		}
		peer, cost := candidate(key)
		present := false
		best := ^uint16(0)
		for _, link := range view.Links {
			if peer != (meshid.Device{}) && link.Device == peer && !link.Retiring {
				present = true
				if link.ForwardingReady && link.Cost < best {
					best = link.Cost
				}
			}
		}
		if !present {
			return 0
		}
		if best == ^uint16(0) || cost < best {
			return 1
		}
		return 2
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}

func (q *CandidateQueue) Order(keys []string, now time.Time) []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	for k, t := range q.failed {
		if now.Sub(t) >= FailedConnectPenalty {
			delete(q.failed, k)
		}
	}
	out := append([]string(nil), keys...)
	shuffle := q.shuffle
	if shuffle == nil {
		shuffle = rand.Shuffle
	}
	shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	sort.SliceStable(out, func(i, j int) bool {
		a, aFailed := q.failed[out[i]]
		b, bFailed := q.failed[out[j]]
		if aFailed != bFailed {
			return !aFailed
		}
		return aFailed && a.Before(b)
	})
	return out
}
