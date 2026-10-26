//go:build linux

package localmesh

import (
	"encoding/binary"
	"sync"
	"time"
)

const (
	// Budget about 150 ms of user-space bytes at the observed write rate.
	// Reserve room for a two-packet Initial flight alongside replies from
	// the same device's existing app flows. Per-source fairness groups all
	// those flows together; sizing only for an otherwise quiet source can
	// repeatedly lose the same ClientHello suffix. The floor and maximum are
	// twice the former limits; the age cap and admitted ordering still apply.
	// Socket/active bytes are separate from this waiting-packet budget.
	streamIPByteBudget       = 16 * 1024
	streamIPFullPacketBytes  = TunnelMTU + 2*(fragmentHeader+5)
	streamIPMinByteBudget    = 4 * (2*streamIPFullPacketBytes + streamFastIPMax + fragmentHeader + 5)
	streamIPSourceByteBudget = streamIPByteBudget / 2
	streamIPInitialQueueAge  = 2 * time.Second
	streamIPMaxQueueAge      = 5 * time.Second
)

type streamIPPacket struct {
	sequence uint32
	data     []byte
	source   [16]byte
	bytes    int
	fast     bool
	queued   time.Time
	grace    time.Duration // Never shorten a waiting packet's measured allowance.
}

type streamIPSendQueue struct {
	mu                   sync.Mutex
	pending              []streamIPPacket
	stats                IPQueueStats
	lastFast             [16]byte
	lastBulk             [16]byte
	wake                 chan struct{}
	writeRateBytesPerSec int
}

func newStreamIPSendQueue() *streamIPSendQueue {
	return &streamIPSendQueue{wake: make(chan struct{}, 1)}
}

func streamIPFramedBytes(n int) int {
	fragments := 1
	if n > DatagramLimit-fragmentHeader {
		fragments = 2
	}
	return n + fragments*(fragmentHeader+5)
}

func (q *streamIPSendQueue) enqueue(sequence uint32, data []byte, now time.Time) bool {
	if !validIP(data) || data[0]>>4 != 6 {
		return false
	}
	source := [16]byte(data[8:24])
	p := streamIPPacket{sequence: sequence, data: append([]byte(nil), data...), source: source, bytes: streamIPFramedBytes(len(data)), fast: streamFastRoutedIP(data), queued: now}
	q.mu.Lock()
	q.expire(now)
	p.grace = q.queueAgeLimit()
	for q.stats.Bytes+p.bytes > q.byteBudget() || q.sourceBytes(p.source)+p.bytes > q.sourceByteBudget() || p.fast && q.fastBytes()+p.bytes > q.fastByteBudget() {
		victim := q.victim(p)
		if victim < 0 {
			q.stats.DroppedCapacity++
			q.mu.Unlock()
			return false
		}
		// Preserve the earliest waiting data in a TCP flow. Replacing it
		// with a later segment creates a receive hole on every burst. An
		// earlier retransmission may still replace later queued data.
		if tcpQueuedBefore(q.pending[victim].data, p.data) {
			q.stats.DroppedCapacity++
			q.mu.Unlock()
			return false
		}
		q.remove(victim)
		q.stats.DroppedCapacity++
	}
	// Keep a retransmitted TCP prefix ahead of later data from the same
	// flow even when both fit. Previously the one-packet minimum happened to
	// enforce this only by replacing the later segment at capacity. Other
	// flows and opaque QUIC datagrams retain their admitted relative order.
	index := len(q.pending)
	for i, queued := range q.pending {
		if queued.fast == p.fast && tcpQueuedBefore(p.data, queued.data) {
			index = i
			break
		}
	}
	q.pending = append(q.pending, streamIPPacket{})
	copy(q.pending[index+1:], q.pending[index:])
	q.pending[index] = p
	q.stats.Bytes += p.bytes
	q.stats.Packets++
	q.stats.Enqueued++
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// tcpQueueSegment classifies only complete TCP data segments with base IP
// headers. This is a queue hint, never an admission or authorization check.
// IPv4-in-IPv6 is used by authenticated Internet leases; keep its inner flow
// tuple so unrelated Cloud connections do not share sequence ordering.
func tcpQueueSegment(packet []byte) (key [45]byte, sequence uint32, ok bool) {
	if len(packet) < 60 || packet[0]>>4 != 6 {
		return
	}
	copy(key[:32], packet[8:40])
	key[32] = packet[6]
	offset := 40
	switch packet[6] {
	case 6:
	case 4:
		if len(packet) < 80 || packet[40] != 0x45 || packet[49] != 6 ||
			int(binary.BigEndian.Uint16(packet[42:44])) != len(packet)-40 ||
			binary.BigEndian.Uint16(packet[46:48])&0xbfff != 0 {
			return
		}
		copy(key[33:41], packet[52:60])
		offset = 60
	default:
		return
	}
	header := int(packet[offset+12]>>4) * 4
	if header < 20 || offset+header >= len(packet) {
		return
	}
	copy(key[41:], packet[offset:offset+4])
	return key, binary.BigEndian.Uint32(packet[offset+4 : offset+8]), true
}

func tcpQueuedBefore(queued, incoming []byte) bool {
	oldKey, oldSeq, oldOK := tcpQueueSegment(queued)
	newKey, newSeq, newOK := tcpQueueSegment(incoming)
	return oldOK && newOK && oldKey == newKey && int32(newSeq-oldSeq) > 0
}

func (q *streamIPSendQueue) byteBudget() int {
	if q.writeRateBytesPerSec == 0 {
		return streamIPMinByteBudget
	}
	return min(streamIPByteBudget, max(streamIPMinByteBudget, q.writeRateBytesPerSec*150/1000))
}

func (q *streamIPSendQueue) sourceByteBudget() int {
	return max(streamIPFramedBytes(TunnelMTU), q.byteBudget()/2)
}

func (q *streamIPSendQueue) fastByteBudget() int {
	return q.byteBudget() - streamIPFramedBytes(TunnelMTU)
}

// QUIC's Initial and TLS flight can fill an entire IP packet. During a
// same-source ICMP flood they otherwise compete for the bulk
// capacity. Reserve bounded fast capacity for the end-to-end app session UDP port, in either
// direction. The remaining full-size slot and the four-fast burst limit keep
// bulk traffic moving. Only inspect UDP immediately after the IPv6 base header. Extension
// headers, including fragmentation, remain in the ordinary size-based lane.
func streamFastRoutedIP(packet []byte) bool {
	if !validIP(packet) || packet[0]>>4 != 6 {
		return false
	}
	if len(packet) <= streamFastIPMax {
		return true
	}
	const header = 40
	if packet[6] != 17 || len(packet) < header+8 ||
		int(binary.BigEndian.Uint16(packet[header+4:header+6])) != len(packet)-header {
		return false
	}
	return binary.BigEndian.Uint16(packet[header:header+2]) == AppSessionPort ||
		binary.BigEndian.Uint16(packet[header+2:header+4]) == AppSessionPort
}

func (q *streamIPSendQueue) fastBytes() int {
	bytes := 0
	for _, p := range q.pending {
		if p.fast {
			bytes += p.bytes
		}
	}
	return bytes
}

func (q *streamIPSendQueue) sourceBytes(source [16]byte) int {
	bytes := 0
	for _, p := range q.pending {
		if p.source == source {
			bytes += p.bytes
		}
	}
	return bytes
}

func (q *streamIPSendQueue) victim(incoming streamIPPacket) int {
	needFastSpace := incoming.fast && q.fastBytes()+incoming.bytes > q.fastByteBudget()
	// TCP retransmissions can arrive after a later segment. Preserve the
	// earliest logical prefix in that flow, including when it arrives later.
	// Verify that removing the flow's later segments can admit the prefix
	// before discarding any: a larger retransmission may still not fit.
	victim, reclaim, reclaimFast := -1, 0, 0
	for i := len(q.pending) - 1; i >= 0; i-- {
		p := q.pending[i]
		if !tcpQueuedBefore(incoming.data, p.data) {
			continue
		}
		reclaim += p.bytes
		if p.fast {
			reclaimFast += p.bytes
		}
		// Evicting bulk cannot relieve a full fast lane.
		if victim < 0 && (!needFastSpace || p.fast) {
			victim = i
		}
	}
	if victim >= 0 && q.stats.Bytes+incoming.bytes-reclaim <= q.byteBudget() &&
		q.sourceBytes(incoming.source)+incoming.bytes-reclaim <= q.sourceByteBudget() &&
		(!incoming.fast || q.fastBytes()+incoming.bytes-reclaimFast <= q.fastByteBudget()) {
		return victim
	}
	if reclaim != 0 {
		return -1 // Preserve the admitted suffix when its prefix cannot fit.
	}
	// Preserve each sender's admitted prefix within a priority class. Replacing
	// it with each later datagram can discard the start of every TLS flight.
	// Existing priority reservation may displace bulk tails, but an arrival
	// from a different sender must not discard another flight's opening bytes.
	source := incoming.source
	needSourceSpace := q.sourceBytes(source)+incoming.bytes > q.sourceByteBudget()
	ownClassBytes := 0
	for _, p := range q.pending {
		if p.source == source && p.fast == incoming.fast {
			ownClassBytes += p.bytes
		}
	}
	if needSourceSpace {
		// Do not discard bulk unless doing so can actually admit this packet.
		if !incoming.fast || needFastSpace || ownClassBytes+incoming.bytes > q.sourceByteBudget() {
			return -1
		}
		for i := len(q.pending) - 1; i >= 0; i-- {
			p := q.pending[i]
			if p.source == source && !p.fast {
				return i
			}
		}
		return -1
	}
	if ownClassBytes != 0 {
		return -1
	}
	if incoming.fast && !needFastSpace {
		bulk := 0
		for _, p := range q.pending {
			if !p.fast {
				bulk++
			}
		}
		if bulk > 1 {
			for i := len(q.pending) - 1; i >= 0; i-- {
				if !q.pending[i].fast {
					return i
				}
			}
		}
	}
	return -1
}

func (q *streamIPSendQueue) expire(now time.Time) {
	limit := q.queueAgeLimit()
	for i := 0; i < len(q.pending); {
		// A later short write may enter the kernel immediately even while RF
		// and intervening control frames remain contended. Do not retroactively
		// revoke a packet's allowance. A measured slowdown can extend it, still
		// relative to its original enqueue time and within the fixed maximum.
		q.pending[i].grace = max(q.pending[i].grace, limit)
		if now.Sub(q.pending[i].queued) >= q.pending[i].grace {
			q.remove(i)
			q.stats.DroppedExpired++
		} else {
			i++
		}
	}
}

func (q *streamIPSendQueue) queueAgeLimit() time.Duration {
	// Before the first write completes there is no drain-rate estimate. The
	// conservative startup bound applies; the fixed byte
	// cap still bounds memory and latency behind the active packet.
	if q.writeRateBytesPerSec == 0 {
		return streamIPInitialQueueAge
	}
	// A waiting packet needs an opportunity to follow the active packet and
	// the bounded queue ahead of it. At low BLE rates even one packet can
	// take hundreds of milliseconds, exceeding the former 50ms age limit.
	service := time.Duration(q.byteBudget()+streamIPFullPacketBytes) * time.Second / time.Duration(q.writeRateBytesPerSec)
	// Reuse startup grace as a floor: socket-fast writes do not predict a
	// queued packet's service opportunity behind a measured 1.5s control write.
	return min(streamIPMaxQueueAge, max(streamIPInitialQueueAge, service))
}

// Write completion is only a proxy for RF drain because the CoC socket can
// buffer data. The small socket send buffer forces backpressure early. A slow
// sample reduces the byte budget immediately; faster samples rise gradually.
func (q *streamIPSendQueue) recordWrite(bytes int, elapsed time.Duration) {
	if elapsed <= 0 {
		return
	}
	rate := min(100*1024, int(float64(bytes)/elapsed.Seconds()))
	q.mu.Lock()
	if q.writeRateBytesPerSec == 0 {
		q.writeRateBytesPerSec = min(rate, 16*1024)
	} else if rate < q.writeRateBytesPerSec {
		q.writeRateBytesPerSec = rate
	} else {
		// A write fitting in the socket buffer can complete almost instantly
		// even on a slow radio. Do not let one such sample erase the measured
		// service time and expire another sender's already waiting packet.
		q.writeRateBytesPerSec = min(rate, q.writeRateBytesPerSec+max(1, q.writeRateBytesPerSec/4))
	}
	q.expire(time.Now())
	for q.stats.Bytes > q.byteBudget() || q.fastBytes() > q.fastByteBudget() {
		victim := len(q.pending) - 1
		if q.fastBytes() > q.fastByteBudget() {
			for i := len(q.pending) - 1; i >= 0; i-- {
				p := q.pending[i]
				if p.fast {
					victim = i
					break
				}
			}
		} else {
			for i := len(q.pending) - 1; i >= 0; i-- {
				p := q.pending[i]
				if !p.fast {
					victim = i
					break
				}
			}
		}
		q.remove(victim)
		q.stats.DroppedCapacity++
	}
	q.mu.Unlock()
}

func (q *streamIPSendQueue) remove(i int) streamIPPacket {
	p := q.pending[i]
	copy(q.pending[i:], q.pending[i+1:])
	q.pending[len(q.pending)-1] = streamIPPacket{}
	q.pending = q.pending[:len(q.pending)-1]
	q.stats.Bytes -= p.bytes
	q.stats.Packets--
	return p
}

func (q *streamIPSendQueue) pop(now time.Time, fastServed *int) (streamIPPacket, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.expire(now)
	if len(q.pending) == 0 {
		return streamIPPacket{}, false
	}
	fast := *fastServed < streamFastIPBurst
	index := q.nextIndex(fast)
	if index < 0 {
		fast = !fast
		index = q.nextIndex(fast)
	}
	p := q.remove(index)
	q.stats.Dequeued++
	waitMS := uint64(max(0, now.Sub(p.queued).Milliseconds()))
	q.stats.LastDequeuedWaitMS = waitMS
	q.stats.MaxDequeuedWaitMS = max(q.stats.MaxDequeuedWaitMS, waitMS)
	if fast {
		*fastServed++
		q.lastFast = p.source
	} else {
		*fastServed = 0
		q.lastBulk = p.source
	}
	return p, true
}

func (q *streamIPSendQueue) nextIndex(fast bool) int {
	last := q.lastBulk
	if fast {
		last = q.lastFast
	}
	fallback := -1
	for i, p := range q.pending {
		if p.fast != fast {
			continue
		}
		if fallback < 0 {
			fallback = i
		}
		if p.source != last {
			return i
		}
	}
	return fallback
}

func (q *streamIPSendQueue) snapshot() IPQueueStats {
	q.mu.Lock()
	defer q.mu.Unlock()
	stats := q.stats
	stats.AgeLimitMS = uint64(q.queueAgeLimit().Milliseconds())
	stats.WriteRateBytesPerSec = q.writeRateBytesPerSec
	stats.ByteLimit = q.byteBudget()
	return stats
}
