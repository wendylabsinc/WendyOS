package babel

import (
	"net/netip"
	"slices"
	"time"

	"github.com/wendylabsinc/WendyOS/babel/internal/wire"
)

type wireMessage = wire.Message
type routeKey struct {
	prefix netip.Prefix
	link   LinkID
}
type sourceKey struct {
	prefix netip.Prefix
	id     RouterID
}
type source struct {
	seq, metric uint16
	expires     time.Duration
}
type candidate struct {
	routeKey
	id                RouterID
	seq, advertised   uint16
	hop               netip.Addr
	expires, interval time.Duration
	refreshRequested  bool
}
type helloState struct {
	seq     uint16
	seen    bool
	expires time.Duration
}
type linkState struct {
	Link
	hellos                                     [2]helloState
	tx                                         uint16
	ihuExpires, nextHello, nextIHU, nextUpdate time.Duration
	helloSeq                                   uint16
	lastDump                                   time.Duration
	dumped                                     bool
}
type request struct {
	message       wireMessage
	targets       []LinkID
	from          LinkID
	next, expires time.Duration
	remaining     int
}

type Engine struct {
	cfg               Config
	now               time.Duration
	seq               uint16
	rng               uint64
	lastLink          LinkID
	links             map[LinkID]*linkState
	routes            map[routeKey]*candidate
	sources           map[sourceKey]*source
	local             map[netip.Prefix]uint16
	selected          map[netip.Prefix]Route
	drops             map[netip.Prefix]time.Duration
	requests          map[sourceKey]*request
	out               map[LinkID][]wireMessage
	revision, pending uint64
	held              []Datagram
	stopped           bool
	counters          Counters
	triggerAt         time.Duration
	trigger           bool
	repeats           int
}

// Step consumes one event. Call due timers using Tick; events at the same time
// are processed in caller order, after due expiry. Caller must serialize calls.
func (e *Engine) Step(now time.Duration, event Event) (Effects, error) {
	if e.stopped {
		return Effects{}, ErrStopped
	}
	if e.pending != 0 {
		return Effects{}, ErrPending
	}
	if now < e.now {
		return Effects{}, ErrTime
	}
	if now > time.Duration(1<<63-1)-48*time.Hour {
		return Effects{}, ErrTime
	}
	if err := e.validate(event); err != nil {
		return Effects{}, err
	}
	old := e.routeSnapshot()
	e.now = now
	e.expire()
	switch v := event.(type) {
	case AddLink:
		l := v.Link
		if l.MaxPacket == 0 {
			l.MaxPacket = 1200
		}
		e.links[l.ID] = &linkState{Link: l, tx: Infinity, nextHello: now, nextIHU: now, nextUpdate: now}
		e.lastLink = l.ID
		e.queue(l.ID, wireMessage{Type: wire.Request, Wildcard: true})
	case RemoveLink:
		delete(e.links, v.ID)
		delete(e.out, v.ID)
		for k, r := range e.routes {
			if k.link == v.ID {
				r.advertised = Infinity
				r.expires = now + hold(e.cfg.UpdateInterval)
			}
		}
	case SetCost:
		if l := e.links[v.ID]; l != nil {
			l.Cost = v.Cost
		}
	case Receive:
		if l := e.links[v.ID]; l != nil {
			e.counters.Received++
			if len(v.Packet) > l.MaxPacket {
				e.counters.Malformed++
				break
			}
			messages, err := wire.Decode(v.Packet, l.Peer)
			if err != nil {
				e.counters.Malformed++
				break
			}
			for _, m := range messages {
				// A packet is one atomic input. Batch consecutive advertisements;
				// requests must see all preceding changes before answering.
				if m.Type == wire.Request || m.Type == wire.SeqRequest {
					e.selectRoutes()
				}
				e.receive(l, m)
			}
		} else {
			e.counters.Stale++
		}
	case Originate:
		e.local[v.Prefix] = v.Metric
		delete(e.drops, v.Prefix)
	case Withdraw:
		delete(e.local, v.Prefix)
	}
	e.selectRoutes()
	e.timers()
	// Source history updates are conservative even if the application drops
	// the corresponding packet. Never forget history merely because a send failed.
	packets := e.flush()
	routes := e.routeSnapshot()
	if !slices.Equal(old, routes) {
		e.revision++
		e.pending = e.revision
		e.held = packets
		return Effects{Revision: e.pending, Routes: routes}, nil
	}
	return Effects{Datagrams: packets}, nil
}

// Commit releases advertisements only after successful forwarding application.
// Failure is terminal: the caller must stop forwarding and discard this engine.
// No further Step is accepted while a commit is outstanding; do not queue stale
// timers behind a slow FIB operation. An in-memory forwarder should swap atomically.
func (e *Engine) Commit(revision uint64, success bool) (Effects, error) {
	if e.stopped {
		return Effects{}, ErrStopped
	}
	if revision == 0 || revision != e.pending {
		return Effects{}, ErrRevision
	}
	e.pending = 0
	if !success {
		e.stopped = true
		e.held = nil
		return Effects{}, ErrStopped
	}
	out := e.held
	e.held = nil
	return Effects{Datagrams: out}, nil
}

func (e *Engine) validate(event Event) error {
	switch v := event.(type) {
	case Tick:
	case AddLink:
		l := v.Link
		if l.ID == 0 || l.ID <= e.lastLink || !l.Local.Is6() || !l.Peer.Is6() || !l.Local.IsLinkLocalUnicast() || !l.Peer.IsLinkLocalUnicast() || l.Local == l.Peer || l.Local.Zone() != "" || l.Peer.Zone() != "" || l.Cost == 0 {
			return ErrConfig
		}
		if l.MaxPacket != 0 && (l.MaxPacket < 512 || l.MaxPacket > 65487) {
			return ErrConfig
		}
		if l.IPv4.IsValid() && (!l.IPv4.Is4() || l.IPv4.IsUnspecified() || l.IPv4.IsMulticast()) {
			return ErrConfig
		}
		if len(e.links) >= e.cfg.MaxLinks {
			return ErrLimit
		}
	case RemoveLink:
	case SetCost:
		if v.Cost == 0 {
			return ErrConfig
		}
	case Receive:
	case Originate:
		if !routablePrefix(v.Prefix) || v.Metric == Infinity {
			return ErrConfig
		}
		if _, ok := e.local[v.Prefix]; !ok && len(e.local)+len(e.routes)+len(e.drops) >= e.cfg.MaxRoutes {
			return ErrLimit
		}
		if _, ok := e.sources[sourceKey{v.Prefix, e.cfg.RouterID}]; !ok && len(e.sources) >= e.cfg.MaxSources {
			return ErrLimit
		}
	case Withdraw:
		if !validPrefix(v.Prefix) {
			return ErrConfig
		}
	default:
		return ErrConfig
	}
	return nil
}

func (e *Engine) cost(l *linkState) uint16 {
	if l == nil || l.ihuExpires <= e.now || l.tx == Infinity {
		return Infinity
	}
	if l.hellos[0].expires <= e.now && l.hellos[1].expires <= e.now {
		return Infinity
	}
	return max(l.Cost, max(l.tx, 1))
}
func (e *Engine) feasible(r *candidate) bool {
	if r.advertised == Infinity {
		return true
	}
	s := e.sources[sourceKey{r.prefix, r.id}]
	return s == nil || newer(r.seq, s.seq) || (r.seq == s.seq && r.advertised < s.metric)
}
func (e *Engine) metric(r *candidate) uint16 {
	if e.cfg.AcceptRoute != nil && !e.cfg.AcceptRoute(r.id, r.prefix) {
		return Infinity
	}
	return sum(r.advertised, e.cost(e.links[r.link]))
}

func (e *Engine) receive(l *linkState, m wireMessage) {
	switch m.Type {
	case wire.Hello:
		i := 0
		if m.Unicast {
			i = 1
		}
		h := &l.hellos[i]
		if !h.seen || h.expires <= e.now || newer(m.Seqno, h.seq) {
			h.seq = m.Seqno
			h.seen = true
			if m.Interval > 0 {
				h.expires = e.now + hold(time.Duration(m.Interval)*10*time.Millisecond)
			}
		}
	case wire.IHU:
		if !m.Address.IsValid() || m.Address == l.Local || m.Address == l.IPv4 {
			l.tx = m.Metric
			l.ihuExpires = e.now + hold(time.Duration(m.Interval)*10*time.Millisecond)
		}
	case wire.AckRequest:
		e.queue(l.ID, wireMessage{Type: wire.Ack, Opaque: m.Opaque})
	case wire.Update:
		e.update(l, m)
	case wire.Request:
		if m.Wildcard {
			if !l.dumped || e.now-l.lastDump >= e.cfg.HelloInterval {
				e.dump(l.ID, true)
				l.lastDump = e.now
				l.dumped = true
			}
		} else {
			e.advertise(l.ID, m.Prefix, true)
		}
	case wire.SeqRequest:
		e.seqRequest(l.ID, m)
	}
}

func (e *Engine) update(l *linkState, m wireMessage) {
	if m.Wildcard {
		for k, r := range e.routes {
			if k.link == l.ID {
				r.advertised = Infinity
			}
		}
		return
	}
	k := routeKey{m.Prefix, l.ID}
	r := e.routes[k]
	if m.Metric == Infinity {
		if r != nil {
			r.advertised = Infinity
		}
		return
	}
	if !routablePrefix(m.Prefix) {
		e.counters.Rejected++
		return
	}
	if e.cfg.AcceptRoute != nil && !e.cfg.AcceptRoute(RouterID(m.RouterID), m.Prefix) {
		e.counters.Rejected++
		return
	}
	if RouterID(m.RouterID) == e.cfg.RouterID {
		e.counters.Rejected++
		return
	}
	if m.V4ViaV6 && !e.cfg.IPv4ViaIPv6 {
		e.counters.Rejected++
		return
	}
	// This profile has one peer per link; arbitrary third-party IPv6 next hops
	// are not reachable. Native IPv4 next hops are carried for the adapter.
	if m.Address.Is6() && m.Address != l.Peer {
		e.counters.Rejected++
		return
	}
	sk := sourceKey{m.Prefix, RouterID(m.RouterID)}
	if _, ok := e.sources[sk]; !ok && len(e.sources) >= e.cfg.MaxSources {
		e.counters.Rejected++
		return
	}
	if r == nil {
		if len(e.routes)+len(e.local)+len(e.drops) >= e.cfg.MaxRoutes {
			e.counters.Rejected++
			return
		}
		r = &candidate{routeKey: k}
		e.routes[k] = r
	}
	if r.id != 0 && r.id != RouterID(m.RouterID) {
		e.scheduleTrigger(true)
	}
	r.id = RouterID(m.RouterID)
	r.seq = m.Seqno
	r.advertised = m.Metric
	r.hop = m.Address
	r.interval = time.Duration(m.Interval) * 10 * time.Millisecond
	r.expires = e.now + hold(r.interval)
	r.refreshRequested = false
	if !e.feasible(r) {
		s := e.sources[sk]
		selected, ok := e.selected[m.Prefix]
		if !ok || selected.Unreachable || selected.Link == l.ID || e.metric(r) < selected.Metric {
			e.request(sk, s.seq+1, []LinkID{l.ID}, 0, 64)
		}
	}
}

func (e *Engine) selectRoutes() {
	old := e.selected
	next := map[netip.Prefix]Route{}
	for p, m := range e.local {
		next[p] = Route{Prefix: p, RouterID: e.cfg.RouterID, Seqno: e.seq, Metric: m, Local: true}
	}
	for _, k := range e.routeKeys() {
		r := e.routes[k]
		if _, local := e.local[k.prefix]; local {
			continue
		}
		metric := e.metric(r)
		if metric == Infinity || !e.feasible(r) {
			continue
		}
		v, ok := next[k.prefix]
		previous := old[k.prefix]
		if !ok || metric < v.Metric || (metric == v.Metric && r.link == previous.Link) {
			next[k.prefix] = Route{Prefix: k.prefix, RouterID: r.id, Seqno: r.seq, Metric: metric, Link: r.link, NextHop: r.hop}
		}
	}
	for _, was := range e.routeSnapshot() {
		p := was.Prefix
		if _, ok := next[p]; !ok && !was.Unreachable {
			e.drops[p] = e.now + hold(e.cfg.UpdateInterval)
			e.scheduleTrigger(true)
			if s := e.sources[sourceKey{p, was.RouterID}]; s != nil {
				var targets []LinkID
				for _, k := range e.routeKeys() {
					r := e.routes[k]
					if k.prefix == p && r.advertised != Infinity && e.cost(e.links[k.link]) != Infinity {
						targets = append(targets, k.link)
					}
				}
				if len(targets) > 0 {
					e.request(sourceKey{p, was.RouterID}, s.seq+1, targets, 0, 64)
				}
			}
		}
	}
	for p, until := range e.drops {
		if _, ok := next[p]; ok {
			delete(e.drops, p)
		} else if until > e.now {
			next[p] = Route{Prefix: p, Metric: Infinity, Unreachable: true}
		} else {
			delete(e.drops, p)
		}
	}
	e.selected = next
	for p, r := range next {
		was, ok := old[p]
		if !ok || was.RouterID != r.RouterID || was.Unreachable != r.Unreachable {
			e.scheduleTrigger(true)
		} else if was.Metric != r.Metric {
			e.scheduleTrigger(false)
		}
	}
	for _, sk := range e.requestKeys() {
		q := e.requests[sk]
		if r, ok := next[sk.prefix]; ok && !r.Unreachable && (r.RouterID != sk.id || atLeast(r.Seqno, q.message.Seqno)) {
			if q.from != 0 {
				e.advertise(q.from, sk.prefix, true)
			}
			delete(e.requests, sk)
		}
	}
}

func (e *Engine) scheduleTrigger(repeat bool) {
	if !e.trigger {
		e.trigger = true
		e.triggerAt = e.now + e.jitter(50*time.Millisecond)
	}
	if repeat {
		e.repeats = 2
	}
}
func (e *Engine) jitter(maximum time.Duration) time.Duration {
	e.rng ^= e.rng << 13
	e.rng ^= e.rng >> 7
	e.rng ^= e.rng << 17
	return time.Duration(e.rng % uint64(maximum+1))
}
func (e *Engine) queue(id LinkID, m wireMessage) {
	if e.links[id] != nil {
		e.out[id] = append(e.out[id], m)
	}
}

func (e *Engine) advertise(id LinkID, p netip.Prefix, explicit bool) {
	l := e.links[id]
	if l == nil {
		return
	}
	r, ok := e.selected[p]
	if !ok || r.Unreachable {
		e.queue(id, wireMessage{Type: wire.Update, Prefix: p, Metric: Infinity, Interval: centis(e.cfg.UpdateInterval)})
		return
	}
	if !explicit && r.Link == id {
		return
	} // split horizon, but never suppress explicit replies
	m := wireMessage{Type: wire.Update, Prefix: p, RouterID: uint64(r.RouterID), Seqno: r.Seqno, Metric: r.Metric, Interval: centis(e.cfg.UpdateInterval)}
	if p.Addr().Is4() {
		if l.IPv4.IsValid() {
			m.Address = l.IPv4
		} else if e.cfg.IPv4ViaIPv6 {
			m.V4ViaV6 = true
		} else {
			return
		}
	}
	sk := sourceKey{p, r.RouterID}
	s := e.sources[sk]
	if s == nil {
		if len(e.sources) >= e.cfg.MaxSources {
			e.counters.Rejected++
			return
		}
		s = &source{seq: r.Seqno, metric: r.Metric}
		e.sources[sk] = s
	}
	if newer(r.Seqno, s.seq) || (r.Seqno == s.seq && r.Metric < s.metric) {
		s.seq = r.Seqno
		s.metric = r.Metric
	}
	s.expires = e.now + e.cfg.SourceGC
	e.queue(id, m)
}
func (e *Engine) dump(id LinkID, explicit bool) {
	for _, r := range e.routeSnapshot() {
		e.advertise(id, r.Prefix, explicit)
	}
}
func centis(d time.Duration) uint16 { return uint16(d / (10 * time.Millisecond)) }

func (e *Engine) request(sk sourceKey, seq uint16, targets []LinkID, from LinkID, hops uint8) {
	if q := e.requests[sk]; q != nil && atLeast(q.message.Seqno, seq) {
		return
	}
	if len(e.requests) >= e.cfg.MaxRequests {
		e.counters.Rejected++
		return
	}
	m := wireMessage{Type: wire.SeqRequest, Prefix: sk.prefix, RouterID: uint64(sk.id), Seqno: seq, HopCount: hops}
	targets = slices.Clone(targets)
	slices.Sort(targets)
	targets = slices.Compact(targets)
	for _, id := range targets {
		e.queue(id, m)
	}
	e.requests[sk] = &request{message: m, targets: targets, from: from, next: e.now + time.Second, expires: e.now + 4*time.Second, remaining: 2}
}
func (e *Engine) seqRequest(from LinkID, m wireMessage) {
	r, ok := e.selected[m.Prefix]
	if ok && !r.Unreachable {
		if uint64(r.RouterID) != m.RouterID || atLeast(r.Seqno, m.Seqno) {
			e.advertise(from, m.Prefix, true)
			return
		}
		if r.Local {
			e.seq++
			e.selectRoutes()
			e.advertise(from, m.Prefix, true)
			return
		}
	}
	if RouterID(m.RouterID) == e.cfg.RouterID || m.HopCount < 2 {
		return
	}
	// Only forward requests for prefixes we have advertised (finite source history).
	advertised := false
	for sk := range e.sources {
		if sk.prefix == m.Prefix {
			advertised = true
			break
		}
	}
	if !advertised {
		return
	}
	var best *candidate
	for _, k := range e.routeKeys() {
		c := e.routes[k]
		if k.prefix != m.Prefix || k.link == from || e.metric(c) == Infinity {
			continue
		}
		if best == nil || (e.feasible(c) && !e.feasible(best)) || (e.feasible(c) == e.feasible(best) && e.metric(c) < e.metric(best)) {
			best = c
		}
	}
	if best != nil {
		e.request(sourceKey{m.Prefix, RouterID(m.RouterID)}, m.Seqno, []LinkID{best.link}, from, m.HopCount-1)
	}
}

func (e *Engine) expire() {
	for k, r := range e.routes {
		if r.expires <= e.now {
			if r.advertised == Infinity {
				delete(e.routes, k)
			} else {
				r.advertised = Infinity
				r.expires = e.now + hold(r.interval)
			}
		}
	}
	for sk, s := range e.sources {
		if s.expires <= e.now {
			delete(e.sources, sk)
		}
	}
	for sk, q := range e.requests {
		if q.expires <= e.now {
			delete(e.requests, sk)
		}
	}
}
func (e *Engine) timers() {
	for _, id := range e.linkIDs() {
		l := e.links[id]
		if l.nextHello <= e.now {
			// Multicast-kind Hello over unicast: a Link has exactly one peer.
			// This also interoperates with wired-cost peers that use multicast history.
			e.queue(id, wireMessage{Type: wire.Hello, Seqno: l.helloSeq, Interval: centis(e.cfg.HelloInterval)})
			l.helloSeq++
			l.nextHello = e.now + e.cfg.HelloInterval - e.jitter(e.cfg.HelloInterval/8)
		}
		if l.nextIHU <= e.now {
			rx := l.Cost
			if l.hellos[0].expires <= e.now && l.hellos[1].expires <= e.now {
				rx = Infinity
			}
			e.queue(id, wireMessage{Type: wire.IHU, Metric: rx, Interval: centis(e.cfg.HelloInterval * 2)})
			l.nextIHU = e.now + e.cfg.HelloInterval*2 - e.jitter(e.cfg.HelloInterval/8)
		}
		if l.nextUpdate <= e.now {
			e.dump(id, false)
			l.nextUpdate = e.now + e.cfg.UpdateInterval - e.jitter(e.cfg.UpdateInterval/8)
		}
	}
	if e.trigger && e.triggerAt <= e.now {
		for _, id := range e.linkIDs() {
			e.dump(id, false)
		}
		if e.repeats > 0 {
			e.repeats--
			e.triggerAt = e.now + 100*time.Millisecond + e.jitter(50*time.Millisecond)
		} else {
			e.trigger = false
		}
	}
	for _, sk := range e.requestKeys() {
		q := e.requests[sk]
		if q.remaining > 0 && q.next <= e.now {
			for _, id := range q.targets {
				e.queue(id, q.message)
			}
			q.remaining--
			q.next = e.now + time.Second
		}
	}
	for _, k := range e.routeKeys() {
		r := e.routes[k]
		selected, ok := e.selected[k.prefix]
		if ok && selected.Link == k.link && !r.refreshRequested && r.advertised != Infinity && r.expires-e.now <= r.interval {
			e.queue(k.link, wireMessage{Type: wire.Request, Prefix: k.prefix})
			r.refreshRequested = true
		}
	}
}

func (e *Engine) flush() []Datagram {
	var out []Datagram
	for _, id := range e.linkIDs() {
		for _, p := range wire.Pack(e.out[id], e.links[id].MaxPacket) {
			out = append(out, Datagram{id, p})
			e.counters.Sent++
		}
		delete(e.out, id)
	}
	return out
}

func (e *Engine) NextDeadline() (time.Duration, bool) {
	if e.stopped || e.pending != 0 {
		return 0, false
	}
	var deadline time.Duration
	set := false
	add := func(d time.Duration) {
		if d > e.now && (!set || d < deadline) {
			deadline = d
			set = true
		}
	}
	for _, l := range e.links {
		add(l.nextHello)
		add(l.nextIHU)
		add(l.nextUpdate)
		add(l.ihuExpires)
		for _, h := range l.hellos {
			add(h.expires)
		}
	}
	for _, r := range e.routes {
		add(r.expires)
		if !r.refreshRequested {
			add(r.expires - r.interval)
		}
	}
	for _, s := range e.sources {
		add(s.expires)
	}
	for _, d := range e.drops {
		add(d)
	}
	for _, q := range e.requests {
		add(q.expires)
		if q.remaining > 0 {
			add(q.next)
		}
	}
	if e.trigger {
		if e.triggerAt <= e.now {
			return e.now, true
		}
		add(e.triggerAt)
	}
	return deadline, set
}
func (e *Engine) Snapshot() Snapshot {
	out := Snapshot{Now: e.now, Seqno: e.seq, Routes: e.routeSnapshot(), Sources: len(e.sources), Candidates: len(e.routes), PendingRequests: len(e.requests), Counters: e.counters}
	for _, id := range e.linkIDs() {
		c := e.cost(e.links[id])
		out.Neighbours = append(out.Neighbours, Neighbour{id, c, c != Infinity})
	}
	return out
}
func prefixLess(a, b netip.Prefix) bool {
	if c := a.Addr().Compare(b.Addr()); c != 0 {
		return c < 0
	}
	return a.Bits() < b.Bits()
}
func (e *Engine) routeSnapshot() []Route {
	out := make([]Route, 0, len(e.selected))
	for _, r := range e.selected {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Route) int {
		if a.Prefix == b.Prefix {
			return 0
		}
		if prefixLess(a.Prefix, b.Prefix) {
			return -1
		}
		return 1
	})
	return out
}
func (e *Engine) linkIDs() []LinkID {
	ids := make([]LinkID, 0, len(e.links))
	for id := range e.links {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
func (e *Engine) routeKeys() []routeKey {
	keys := make([]routeKey, 0, len(e.routes))
	for k := range e.routes {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b routeKey) int {
		if a.prefix != b.prefix {
			if prefixLess(a.prefix, b.prefix) {
				return -1
			}
			return 1
		}
		if a.link < b.link {
			return -1
		}
		if a.link > b.link {
			return 1
		}
		return 0
	})
	return keys
}
func (e *Engine) requestKeys() []sourceKey {
	keys := make([]sourceKey, 0, len(e.requests))
	for k := range e.requests {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b sourceKey) int {
		if a.prefix != b.prefix {
			if prefixLess(a.prefix, b.prefix) {
				return -1
			}
			return 1
		}
		if a.id < b.id {
			return -1
		}
		if a.id > b.id {
			return 1
		}
		return 0
	})
	return keys
}
