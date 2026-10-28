// Package netgossip coalesces content-addressed announcement downloads across
// authenticated neighbours. It handles bytes and scheduling, never authorization.
package netgossip

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"sort"
	"sync"
	"time"
)

const (
	Manifest     uint8 = 1
	Service      uint8 = 2
	Version            = 1
	MaxObject          = 40 << 10
	ChunkSize          = 1024
	IdleTimeout        = 30 * time.Second
	HintLifetime       = 2 * time.Minute
	maxKnown           = 4096
	maxPending         = 128
	maxSuppliers       = 64
	maxInFlight        = 16
	maxPerPeer         = 4
	maxBytes           = 8 << 20
)

// Digests and DATA are native CBOR byte strings. DATA is chunked so actual progress, rather than polling, extends
// ownership on slow links. Each request belongs to one session incarnation.
type Message struct {
	Version int    `json:"v" cbor:"v"`
	Op      string `json:"op" cbor:"o"`
	Type    uint8  `json:"type" cbor:"t"`
	Digest  []byte `json:"digest" cbor:"h"`
	Request uint64 `json:"request,omitempty" cbor:"r,omitempty"`
	Offset  int    `json:"offset,omitempty" cbor:"f,omitempty"`
	Total   int    `json:"total,omitempty" cbor:"l,omitempty"`
	Data    []byte `json:"data,omitempty" cbor:"d,omitempty"`
}

// Known separates optional future operations from the formats understood here.
// Unknown operations are consumed without changing hints, claims or authority.
func (m Message) Known() bool {
	switch m.Op {
	case "have", "want", "unavailable", "data", "gone":
		return true
	}
	return false
}

func (m *Message) UnmarshalCBOR(data []byte) error {
	var head struct {
		Op string `json:"op" cbor:"o"`
	}
	if err := netwire.Unmarshal(data, &head); err != nil {
		return err
	}
	if head.Op != "" && !(Message{Op: head.Op}).Known() {
		*m = Message{Op: head.Op}
		return nil
	}
	type plain Message
	var parsed plain
	if err := netwire.Unmarshal(data, &parsed); err != nil {
		return err
	}
	*m = Message(parsed)
	return nil
}

func (m Message) Validate() error {
	if m.Op != "" && !m.Known() {
		return nil
	}
	if m.Version != Version || (m.Type != Manifest && m.Type != Service) || len(m.Digest) != sha256.Size {
		return errors.New("invalid gossip version, type or digest")
	}
	switch m.Op {
	case "have", "gone":
		if m.Request != 0 || m.Offset != 0 || m.Total != 0 || len(m.Data) != 0 {
			return errors.New("invalid HAVE")
		}
	case "want", "unavailable":
		if m.Request == 0 || m.Offset != 0 || m.Total != 0 || len(m.Data) != 0 {
			return errors.New("invalid gossip request")
		}
	case "data":
		if m.Request == 0 || m.Total <= 0 || m.Total > MaxObject || m.Offset < 0 || m.Offset >= m.Total || m.Offset%ChunkSize != 0 || len(m.Data) != min(ChunkSize, m.Total-m.Offset) {
			return errors.New("invalid gossip DATA chunk")
		}
	default:
		return errors.New("unknown gossip operation")
	}
	return nil
}

type ID struct {
	Type uint8
	Hash [32]byte
}

// Identify hashes a deterministic signed envelope, including its signature.
// Forwarders retain the exact origin envelope instead of re-signing it.
func Identify(kind uint8, data []byte) ID {
	h := sha256.New()
	h.Write([]byte("wendy-mesh-artifact/1\x00"))
	h.Write([]byte{kind})
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(data)))
	h.Write(size[:])
	h.Write(data)
	return ID{kind, [32]byte(h.Sum(nil))}
}
func (m Message) ID() ID {
	var hash [32]byte
	copy(hash[:], m.Digest)
	return ID{m.Type, hash}
}
func message(op string, id ID, request uint64) *Message {
	return &Message{Version: Version, Op: op, Type: id.Type, Digest: append([]byte(nil), id.Hash[:]...), Request: request}
}

type object struct {
	data          []byte
	load          func(time.Time) []byte
	expires, used time.Time
}
type fetch struct {
	owner, request uint64
	suppliers      map[uint64]time.Time
	progress       time.Time
	data           []byte
	total          int
	received       bool
	avoid          uint64
}

// Pool is shared by all sessions and both announcement types on one router.
// No callback or network operation runs under its mutex.
type Pool struct {
	mu             sync.Mutex
	objects        map[ID]object
	pending        map[ID]*fetch
	owner, request uint64
	bytes, serving int
}

func NewPool() *Pool { return &Pool{objects: make(map[ID]object), pending: make(map[ID]*fetch)} }

type transfer struct {
	message Message
	data    []byte
	offset  int
}

// Session is owned by its protocol actor. Pool methods serialize cross-link
// claims; Session methods themselves must not be called concurrently.
type Session struct {
	pool        *Pool
	owner       uint64
	kind        uint8
	seen        map[ID]time.Time
	unavailable map[ID]bool
	out         []*transfer
	closed      bool
}

func (p *Pool) NewSession(kind uint8) *Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.owner++
	return &Session{pool: p, owner: p.owner, kind: kind, seen: make(map[ID]time.Time), unavailable: make(map[ID]bool)}
}

func (p *Pool) prune(now time.Time) {
	for id, o := range p.objects {
		if !o.expires.After(now) {
			p.bytes -= len(o.data)
			delete(p.objects, id)
		}
	}
	for id, f := range p.pending {
		for owner, until := range f.suppliers {
			if !until.After(now) {
				delete(f.suppliers, owner)
			}
		}
		if len(f.suppliers) == 0 {
			delete(p.pending, id)
			continue
		}
		if f.owner != 0 && !f.received && now.Sub(f.progress) >= IdleTimeout {
			f.avoid, f.owner = f.owner, 0
			f.data, f.total = nil, 0
		}
	}
}
func (p *Pool) remember(id ID, data []byte, expires, now time.Time) {
	if !expires.After(now) {
		return
	}
	p.prune(now)
	var load func(time.Time) []byte
	if old, ok := p.objects[id]; ok {
		load = old.load
		p.bytes -= len(old.data)
		delete(p.objects, id)
	}
	for len(p.objects) >= maxKnown || p.bytes+len(data) > maxBytes {
		payloadOnly := len(p.objects) < maxKnown
		var oldest ID
		var when time.Time
		found := false
		for k, o := range p.objects {
			if payloadOnly && len(o.data) == 0 {
				continue
			}
			if !found || o.used.Before(when) {
				oldest, when, found = k, o.used, true
			}
		}
		if !found {
			break
		}
		o := p.objects[oldest]
		p.bytes -= len(o.data)
		if payloadOnly {
			o.data = nil
			p.objects[oldest] = o
		} else {
			delete(p.objects, oldest)
		}
	}
	p.objects[id] = object{data: append([]byte(nil), data...), load: load, expires: expires, used: now}
	p.bytes += len(data)
	delete(p.pending, id)
}

// Offer is called only for an independently verified local/relayed object.
// An optional loader recovers evicted bytes by their authoritative store key.
// It must not retain the payload or scan the store; Lookup verifies its digest.
func (s *Session) Offer(data []byte, expires, now time.Time, loaders ...func(time.Time) []byte) *Message {
	if s.closed || len(data) == 0 || len(data) > MaxObject || !expires.After(now) {
		return nil
	}
	id := Identify(s.kind, data)
	s.pool.mu.Lock()
	s.pool.remember(id, data, expires, now)
	if len(loaders) > 0 {
		o := s.pool.objects[id]
		o.load = loaders[0]
		s.pool.objects[id] = o
	}
	s.pool.mu.Unlock()
	return message("have", id, 0)
}

// Lookup recovers only an already verified receipt, never an unknown digest.
// Store access runs outside the pool mutex and costs at most one keyed lookup
// and one envelope hash. Missing or superseded authoritative bodies are ignored.
func (s *Session) Lookup(id ID, now time.Time) []byte {
	if s.closed || id.Type != s.kind {
		return nil
	}
	p := s.pool
	p.mu.Lock()
	p.prune(now)
	o, ok := p.objects[id]
	p.mu.Unlock()
	if !ok {
		return nil
	}
	if len(o.data) > 0 {
		return append([]byte(nil), o.data...)
	}
	if o.load == nil {
		return nil
	}
	data := o.load(now)
	if len(data) == 0 || len(data) > MaxObject || Identify(id.Type, data) != id {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.remember(id, data, o.expires, now)
	return data
}

func Unavailable(request Message) *Message {
	return message("unavailable", request.ID(), request.Request)
}

// Gone proactively withdraws this peer's HAVE for exactly one artifact. It is
// cache residency information, never an origin-signed service withdrawal.
// Send only to peers supporting this prototype extension, after eviction and
// before advertising subsequent availability on that session. Accepted outgoing
// transfers must retain their object; evict an idle object instead.
func Gone(id ID) *Message { return message("gone", id, 0) }

// Resolve runs only after the adapter's existing identity/signature/scope and
// version checks finish. Authenticated stale bodies get a short receipt;
// unverified expired bodies use Discard, and invalid bodies fail their session.
func (s *Session) Resolve(data []byte, expires, now time.Time, accepted bool) {
	id := Identify(s.kind, data)
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	if accepted {
		s.pool.remember(id, data, expires, now)
	} else {
		s.pool.remember(id, nil, now.Add(IdleTimeout), now)
	}
}

// Discard releases unverified bytes without creating a shared receipt. The
// supplier is retried only after a fresh HAVE; alternate suppliers remain free.
func (s *Session) Discard(data []byte) {
	id := Identify(s.kind, data)
	p := s.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if f := p.pending[id]; f != nil && f.owner == s.owner {
		delete(f.suppliers, s.owner)
		f.owner, f.request = 0, 0
		f.data, f.total, f.received = nil, 0, false
		if len(f.suppliers) == 0 {
			delete(p.pending, id)
		}
	}
	delete(s.seen, id)
	delete(s.unavailable, id)
}

func (s *Session) claim(id ID, now time.Time) *Message {
	p := s.pool
	if _, ok := p.objects[id]; ok {
		delete(s.seen, id)
		delete(s.unavailable, id)
		return nil
	}
	if s.unavailable[id] {
		return nil
	}
	f := p.pending[id]
	if f != nil {
		if _, ok := f.suppliers[s.owner]; ok || len(f.suppliers) < maxSuppliers {
			f.suppliers[s.owner] = s.seen[id]
		}
		if f.owner != 0 || (f.avoid == s.owner && len(f.suppliers) > 1) {
			return nil
		}
	}
	all, peer := 0, 0
	for _, v := range p.pending {
		// A completed body still owns its reservation while the adapter
		// verifies it, including any missing-certificate exchange.
		if v.owner != 0 {
			all++
			if v.owner == s.owner {
				peer++
			}
		}
	}
	if all >= maxInFlight || peer >= maxPerPeer {
		return nil
	}
	if f == nil {
		// Unrequested hints already live in the bounded session map. They
		// must not reserve global pending capacity ahead of other neighbours.
		if len(p.pending) >= maxPending {
			return nil
		}
		f = &fetch{suppliers: map[uint64]time.Time{s.owner: s.seen[id]}}
		p.pending[id] = f
	}
	p.request++
	f.owner, f.request, f.progress = s.owner, p.request, now
	return message("want", id, f.request)
}

// Receive returns a protocol reply and, at most once, a complete untrusted
// body for the adapter to authenticate. Unsolicited/obsolete DATA is ignored.
func (s *Session) Receive(m Message, now time.Time) (*Message, []byte, error) {
	return s.receive(m, now, false)
}

// ReceiveDeferred retains an accepted WANT in the existing bounded upload
// slots without advancing its first DATA. Poll emits that DATA only when the
// adapter has bulk output credit. Immediate replies need at most one slot.
func (s *Session) ReceiveDeferred(m Message, now time.Time) (*Message, []byte, error) {
	return s.receive(m, now, true)
}

func (s *Session) receive(m Message, now time.Time, deferUpload bool) (*Message, []byte, error) {
	if err := m.Validate(); err != nil {
		return nil, nil, err
	}
	if !m.Known() {
		return nil, nil, nil
	}
	if s.closed || m.Type != s.kind {
		return nil, nil, errors.New("gossip session type mismatch")
	}
	p := s.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(now)
	id := m.ID()
	switch m.Op {
	case "gone":
		delete(s.seen, id)
		delete(s.unavailable, id)
		if f := p.pending[id]; f != nil {
			delete(f.suppliers, s.owner)
			// Completed bytes may already be under independent verification.
			// Withdrawal cannot release that reservation or discard those bytes.
			if f.owner == s.owner && !f.received {
				f.owner, f.request, f.avoid = 0, 0, s.owner
				f.data, f.total = nil, 0
			}
			if f.owner == 0 && len(f.suppliers) == 0 {
				delete(p.pending, id)
			}
		}
		return nil, nil, nil
	case "have":
		if object, known := p.objects[id]; known {
			object.used = now
			p.objects[id] = object
			return nil, nil, nil
		}
		if _, seen := s.seen[id]; !seen && len(s.seen) >= maxKnown {
			return nil, nil, errors.New("too many announcement hints")
		}
		delete(s.unavailable, id)
		s.seen[id] = now.Add(HintLifetime)
		return s.claim(id, now), nil, nil
	case "want":
		for _, x := range s.out {
			if x.message.Request == m.Request {
				return nil, nil, nil
			}
		}
		o, ok := p.objects[id]
		if !ok || len(o.data) == 0 || len(s.out) >= maxPerPeer || p.serving >= maxInFlight {
			return message("unavailable", id, m.Request), nil, nil
		}
		x := &transfer{message: *message("data", id, m.Request), data: o.data}
		s.out = append(s.out, x)
		p.serving++
		if deferUpload {
			return nil, nil, nil
		}
		return s.chunk(), nil, nil
	case "unavailable":
		if f := p.pending[id]; f != nil && f.owner == s.owner && f.request == m.Request && !f.received {
			delete(f.suppliers, s.owner)
			s.unavailable[id] = true
			f.owner = 0
			f.data = nil
			f.total = 0
		}
	case "data":
		f := p.pending[id]
		if f == nil || f.owner != s.owner || f.request != m.Request || f.received {
			return nil, nil, nil
		}
		if m.Offset != len(f.data) || (f.total != 0 && f.total != m.Total) {
			return nil, nil, errors.New("unordered gossip DATA")
		}
		f.total = m.Total
		f.data = append(f.data, m.Data...)
		f.progress = now
		if len(f.data) == f.total {
			if Identify(s.kind, f.data) != id {
				return nil, nil, errors.New("announcement digest mismatch")
			}
			data := f.data
			f.data = nil
			f.received = true
			return nil, data, nil
		}
	}
	return nil, nil, nil
}

// chunk advances only into the caller's bounded reliable reply queue. Failure
// to admit replies must close/release the session, never silently drop DATA.
func (s *Session) chunk() *Message {
	if len(s.out) == 0 {
		return nil
	}
	x := s.out[0]
	m := x.message
	m.Offset = x.offset
	m.Total = len(x.data)
	end := min(x.offset+ChunkSize, len(x.data))
	m.Data = append([]byte(nil), x.data[x.offset:end]...)
	x.offset = end
	if end == len(x.data) {
		s.out = s.out[1:]
		s.pool.serving--
	} else {
		s.out = append(s.out[1:], x)
	}
	return &m
}

func (s *Session) Poll(now time.Time, slots ...int) []*Message {
	budget := 12
	if len(slots) > 0 {
		budget = min(budget, max(0, slots[0]))
	}
	if s.closed {
		return nil
	}
	p := s.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(now)
	var out []*Message
	for i := 0; i < min(8, budget) && len(s.out) > 0; i++ {
		out = append(out, s.chunk())
	}
	ids := make([]ID, 0, len(s.seen))
	for id := range s.seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i].Hash[:], ids[j].Hash[:]) < 0 })
	for _, id := range ids {
		if !s.seen[id].After(now) {
			delete(s.seen, id)
			delete(s.unavailable, id)
			continue
		}
		if len(out) < budget {
			if m := s.claim(id, now); m != nil {
				out = append(out, m)
			}
		}
	}
	return out
}

// Pending includes advertised objects owned by another session, and bodies
// already downloaded but still awaiting certificate verification.
func (s *Session) Pending(now time.Time) bool {
	p := s.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, until := range s.seen {
		if until.After(now) {
			if _, known := p.objects[id]; !known {
				return true
			}
		}
	}
	return false
}
func (s *Session) Close() {
	if s.closed {
		return
	}
	s.closed = true
	p := s.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	p.serving -= len(s.out)
	s.out = nil
	for id, f := range p.pending {
		delete(f.suppliers, s.owner)
		if f.owner == s.owner {
			f.owner = 0
			f.received = false
			f.data = nil
			f.total = 0
		}
		if len(f.suppliers) == 0 {
			delete(p.pending, id)
		}
	}
	clear(s.seen)
	clear(s.unavailable)
}
