package localmesh

import (
	"encoding/binary"
	"errors"
	"sync"
	"time"
)

const (
	// Router QUIC negotiates the explicit whole-IP receive capability. Stream
	// and leaf roles retain their own independent protocol versions/framing.
	// All roles require signed route origins bound to the complete principal.
	// Require exact ALPN/hello matches; never fall back to weaker protocols.
	LinkALPN            = "wendy-net-router/2"
	LinkProtocolVersion = 2
	// Leave room for an IPv6 outer header around a minimum-size IPv6 packet.
	// Carrier fragmentation remains at most two bounded datagrams per packet.
	TunnelMTU          = 1320
	DatagramLimit      = 1100
	PacketIP      byte = 1
	PacketBabel   byte = 2
	// Routed app sessions use this UDP port on the mesh host VIP. Keep the
	// carrier's bounded admission priority and the app listener in sync.
	AppSessionPort = 43021
	fragmentHeader = 9 // kind, packet ID, total size, offset
)

// EncodeIP fragments one complete IP packet into bounded link datagrams. IDs
// advance once per complete packet on each fresh authenticated link (including
// framed carriers), using uint32 serial order across wrap; no cross-link state.
func EncodeIP(id uint32, packet []byte) ([][]byte, error) {
	if !validIP(packet) {
		return nil, errors.New("invalid tunnel IP packet")
	}
	var out [][]byte
	for offset := 0; offset < len(packet); {
		n := min(DatagramLimit-fragmentHeader, len(packet)-offset)
		d := make([]byte, fragmentHeader+n)
		d[0] = PacketIP
		binary.BigEndian.PutUint32(d[1:5], id)
		binary.BigEndian.PutUint16(d[5:7], uint16(len(packet)))
		binary.BigEndian.PutUint16(d[7:9], uint16(offset))
		copy(d[9:], packet[offset:offset+n])
		out = append(out, d)
		offset += n
	}
	return out, nil
}

func validIP(p []byte) bool {
	if len(p) < 20 || len(p) > TunnelMTU {
		return false
	}
	switch p[0] >> 4 {
	case 4:
		h := int(p[0]&15) * 4
		return h >= 20 && h <= len(p) && int(binary.BigEndian.Uint16(p[2:4])) == len(p)
	case 6:
		return len(p) >= 40 && int(binary.BigEndian.Uint16(p[4:6]))+40 == len(p)
	}
	return false
}

const (
	ipPendingLimit     = 32
	ipPendingByteLimit = ipPendingLimit * TunnelMTU
	ipAssemblyLifetime = 10 * time.Second
	ipRetirementWindow = 1024 // Fixed bookkeeping, independent of peer ID churn.
)

type partialIP struct {
	data          []byte
	first, second bool
	expires       time.Time
	order         uint64
}

// IPAssemblyStats counts hop-local carrier IP work, not app messages or wire
// delivery. Bytes counts received carrier payload bytes, including its header.
// PendingBytes reserves each incomplete packet's full declared size.
type IPAssemblyStats struct {
	Fragments, Bytes                                 uint64
	WholePackets, ReassembledPackets                 uint64
	Duplicates, RetiredFragments                     uint64
	ExpiredPackets, EvictedPackets, DiscardedPackets uint64
	InvalidFragments, ConflictingFragments           uint64
	PendingPackets, PendingBytes, PeakPendingBytes   int
	PacketLimit, ByteLimit                           int
	LifetimeMS                                       uint64
}

// IPAssembler has one receive owner. Snapshot alone may run concurrently; it
// reads a consistent published copy, never the mutable pending map. Do not copy
// an assembler after first use. At most two fixed-position pieces are accepted.
// Missing pieces are ordinary packet loss; capacity must not stall fresh IP.
type IPAssembler struct {
	wholeIP    bool // Set before the owner starts, only after router capability validation.
	pending    map[uint32]partialIP
	bytes      int
	order      uint64
	retirement ipRetirement
	stats      IPAssemblyStats
	snapshotMu sync.Mutex
	published  IPAssemblyStats
}

// ipRetirement uses serial uint32 ordering, including wrap, to reject retired
// IDs without an unbounded tombstone map. IDs outside the window cannot reopen
// discarded state. Existing pending entries are checked before this window so
// later whole packets cannot invalidate a still-live delayed second piece.
type ipRetirement struct {
	initialized bool
	newest      uint32
	bits        [ipRetirementWindow / 64]uint64
}

func (r *ipRetirement) admit(id uint32) bool {
	if !r.initialized {
		r.initialized, r.newest = true, id
	} else if delta := id - r.newest; delta != 0 && delta < 1<<31 {
		if delta >= ipRetirementWindow {
			clear(r.bits[:])
		} else {
			for step := uint32(1); step <= delta; step++ {
				slot := (r.newest + step) % ipRetirementWindow
				r.bits[slot/64] &^= uint64(1) << (slot % 64)
			}
		}
		r.newest = id
	}
	if r.newest-id >= ipRetirementWindow {
		return false
	}
	slot := id % ipRetirementWindow
	return r.bits[slot/64]&(uint64(1)<<(slot%64)) == 0
}

func (r *ipRetirement) retire(id uint32) {
	if r.initialized && r.newest-id < ipRetirementWindow {
		slot := id % ipRetirementWindow
		r.bits[slot/64] |= uint64(1) << (slot % 64)
	}
}

func (a *IPAssembler) publish() {
	s := a.stats
	s.PendingPackets, s.PendingBytes = len(a.pending), a.bytes
	s.PacketLimit, s.ByteLimit = ipPendingLimit, ipPendingByteLimit
	s.LifetimeMS = uint64(ipAssemblyLifetime / time.Millisecond)
	a.snapshotMu.Lock()
	a.published = s
	a.snapshotMu.Unlock()
}

// Snapshot is safe during Receive, Expire and owner shutdown. The returned
// value is independent of future updates and contains no packet data or IDs.
func (a *IPAssembler) Snapshot() IPAssemblyStats {
	a.snapshotMu.Lock()
	s := a.published
	a.snapshotMu.Unlock()
	// Supply limits even before the receive owner has published its first event.
	s.PacketLimit, s.ByteLimit = ipPendingLimit, ipPendingByteLimit
	s.LifetimeMS = uint64(ipAssemblyLifetime / time.Millisecond)
	return s
}

func (a *IPAssembler) remove(id uint32) {
	p := a.pending[id]
	a.bytes -= len(p.data)
	delete(a.pending, id)
	a.retirement.retire(id)
}

// Expire releases buffers at their absolute admission deadline. A duplicate
// never extends that deadline, including on a slow BLE stream.
func (a *IPAssembler) Expire(now time.Time) {
	changed := false
	for id, p := range a.pending {
		if !p.expires.After(now) {
			a.remove(id)
			a.stats.ExpiredPackets++
			changed = true
		}
	}
	if changed {
		a.publish()
	}
}

func (a *IPAssembler) nextExpiry() time.Time {
	var next time.Time
	for _, p := range a.pending {
		if next.IsZero() || p.expires.Before(next) {
			next = p.expires
		}
	}
	return next
}

func (a *IPAssembler) discard() {
	for id := range a.pending {
		a.remove(id)
		a.stats.DiscardedPackets++
	}
	a.publish()
}

func (a *IPAssembler) Receive(d []byte, now time.Time) ([]byte, error) {
	defer a.publish()
	a.Expire(now)
	a.stats.Fragments++
	a.stats.Bytes += uint64(len(d))
	invalid := func(message string) ([]byte, error) {
		a.stats.InvalidFragments++
		return nil, errors.New(message)
	}
	limit := DatagramLimit
	if a.wholeIP {
		limit = WholeIPDatagramLimit
	}
	if len(d) <= fragmentHeader || len(d) > limit || d[0] != PacketIP {
		return invalid("invalid IP datagram")
	}
	id := binary.BigEndian.Uint32(d[1:5])
	total := int(binary.BigEndian.Uint16(d[5:7]))
	offset := int(binary.BigEndian.Uint16(d[7:9]))
	const chunk = DatagramLimit - fragmentHeader
	whole := a.wholeIP && offset == 0 && len(d)-fragmentHeader == total
	if total < 20 || total > TunnelMTU || (!whole && ((offset != 0 && offset != chunk) || offset >= total || len(d)-fragmentHeader != min(chunk, total-offset))) {
		return invalid("invalid IP fragment")
	}
	p, exists := a.pending[id]
	conflict := func(message string) ([]byte, error) {
		a.remove(id)
		a.stats.ConflictingFragments++
		return nil, errors.New(message)
	}
	if exists && (len(p.data) != total || whole) {
		return conflict("conflicting IP fragment length")
	}
	// Validate whole packets before copying. Fragmented packets cannot be fully
	// validated until both halves arrive; their declared allocation is bounded.
	if total <= chunk || whole {
		if !validIP(d[fragmentHeader:]) {
			return invalid("invalid reassembled IP")
		}
		if !a.retirement.admit(id) {
			a.stats.RetiredFragments++
			return nil, nil
		}
		a.retirement.retire(id)
		a.stats.WholePackets++
		return append([]byte(nil), d[fragmentHeader:]...), nil
	}
	if !exists {
		if !a.retirement.admit(id) {
			a.stats.RetiredFragments++
			return nil, nil
		}
		if len(a.pending) >= ipPendingLimit || a.bytes+total > ipPendingByteLimit {
			// Keep the newest complete traffic moving after loss. Admission order is
			// deterministic even if every timestamp is equal; duplicates do not touch it.
			var oldest uint32
			var order uint64
			for key, other := range a.pending {
				if order == 0 || other.order < order {
					oldest, order = key, other.order
				}
			}
			a.remove(oldest)
			a.stats.EvictedPackets++
		}
		if a.pending == nil {
			a.pending = make(map[uint32]partialIP)
		}
		a.order++
		p = partialIP{data: make([]byte, total), expires: now.Add(ipAssemblyLifetime), order: a.order}
		a.bytes += total
		a.stats.PeakPendingBytes = max(a.stats.PeakPendingBytes, a.bytes)
	}
	seen := p.first
	if offset != 0 {
		seen = p.second
	}
	if seen {
		for i, b := range d[fragmentHeader:] {
			if p.data[offset+i] != b {
				return conflict("conflicting duplicate IP fragment")
			}
		}
		a.stats.Duplicates++
		return nil, nil
	}
	copy(p.data[offset:], d[fragmentHeader:])
	if offset == 0 {
		p.first = true
	} else {
		p.second = true
	}
	// Store first so completion and errors share exact accounting.
	a.pending[id] = p
	if p.first && p.second {
		a.remove(id)
		if !validIP(p.data) {
			return invalid("invalid reassembled IP")
		}
		a.stats.ReassembledPackets++
		return p.data, nil
	}
	return nil, nil
}
