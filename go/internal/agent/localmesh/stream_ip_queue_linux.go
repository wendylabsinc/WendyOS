//go:build linux

package localmesh

import (
	"encoding/binary"
	"sync"
	"time"
)

const (
	// Budget about 150 ms of user-space bytes at the observed write rate.
	// Keep room for one full packet from each of two contending senders even
	// at the slowest observed rate. With only one packet slot, a busy sender
	// could evict another sender's packet before the round-robin dequeue saw
	// it. The age limit still drops stale packets before transmission.
	// The socket and active packet are separate and require hardware timing.
	streamIPByteBudget       = 8 * 1024
	streamIPFullPacketBytes  = TunnelMTU + 2*(fragmentHeader+5)
	streamIPMinByteBudget    = 2 * streamIPFullPacketBytes
	streamIPSourceByteBudget = streamIPByteBudget / 2
	streamIPSlowQueueAge     = 50 * time.Millisecond
	streamIPMidQueueAge      = 150 * time.Millisecond
	streamIPFastQueueAge     = 200 * time.Millisecond
)

type streamIPPacket struct {
	sequence uint32
	data     []byte
	source   uint32
	bytes    int
	fast     bool
	queued   time.Time
}

type streamIPSendQueue struct {
	mu                   sync.Mutex
	pending              []streamIPPacket
	stats                IPQueueStats
	lastFast             uint32
	lastBulk             uint32
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
	p := streamIPPacket{sequence: sequence, data: append([]byte(nil), data...), source: binary.BigEndian.Uint32(data[12:16]), bytes: streamIPFramedBytes(len(data)), fast: streamFastRoutedIP(data), queued: now}
	q.mu.Lock()
	q.expire(now)
	for q.stats.Bytes+p.bytes > q.byteBudget() || q.sourceBytes(p.source)+p.bytes > q.sourceByteBudget() || p.fast && q.fastBytes()+p.bytes > q.fastByteBudget() {
		victim := q.victim(p)
		if victim < 0 {
			q.stats.DroppedCapacity++
			q.mu.Unlock()
			return false
		}
		q.remove(victim)
		q.stats.DroppedCapacity++
	}
	q.pending = append(q.pending, p)
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
// same-source ICMP flood they otherwise compete for the same single bulk
// slot and are repeatedly evicted before transmission. Reserve at most one
// full-size fast slot for the end-to-end app session UDP port, in either
// direction. The remaining full-size slot and the four-fast burst limit keep
// bulk traffic moving. Only inspect a complete, unfragmented IPv4 packet.
func streamFastRoutedIP(packet []byte) bool {
	if len(packet) <= streamFastIPMax {
		return true
	}
	if !validIP(packet) || packet[0]>>4 != 4 || packet[9] != 17 ||
		binary.BigEndian.Uint16(packet[6:8])&0x3fff != 0 {
		return false
	}
	header := int(packet[0]&15) * 4
	if len(packet) < header+8 || int(binary.BigEndian.Uint16(packet[header+4:header+6])) != len(packet)-header {
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

func (q *streamIPSendQueue) sourceBytes(source uint32) int {
	bytes := 0
	for _, p := range q.pending {
		if p.source == source {
			bytes += p.bytes
		}
	}
	return bytes
}

func (q *streamIPSendQueue) victim(incoming streamIPPacket) int {
	// Replace old packets from the incoming sender first at its source cap.
	// Otherwise evict from the largest sender in the same priority class.
	// A full-size app QUIC flight is fast: it may replace a bulk packet,
	// but ordinary bulk from the same sender must never evict that flight.
	// The fast byte cap always reserves one maximum-size bulk slot.
	source := incoming.source
	needSourceSpace := q.sourceBytes(source)+incoming.bytes > q.sourceByteBudget()
	needFastSpace := incoming.fast && q.fastBytes()+incoming.bytes > q.fastByteBudget()
	if !needSourceSpace {
		counts := make(map[uint32]int)
		for _, p := range q.pending {
			if p.fast == incoming.fast {
				counts[p.source] += p.bytes
			}
		}
		var largest int
		for _, p := range q.pending {
			if p.source == incoming.source || p.fast != incoming.fast {
				continue
			}
			if bytes := counts[p.source]; bytes > largest {
				largest, source = bytes, p.source
			}
		}
		if largest == 0 {
			source = incoming.source
		}
	}
	for i, p := range q.pending {
		if p.source == source && p.fast == incoming.fast {
			return i
		}
	}
	if needSourceSpace && incoming.fast && !needFastSpace {
		for i, p := range q.pending {
			if p.source == source && !p.fast {
				return i
			}
		}
	}
	if !needSourceSpace && incoming.fast && !needFastSpace {
		bulk := 0
		for _, p := range q.pending {
			if !p.fast {
				bulk++
			}
		}
		if bulk > 1 {
			for i, p := range q.pending {
				if !p.fast {
					return i
				}
			}
		}
	}
	if needSourceSpace && !incoming.fast {
		for i, p := range q.pending {
			if p.source == source && !p.fast {
				return i
			}
		}
	}
	return -1
}

func (q *streamIPSendQueue) expire(now time.Time) {
	for i := 0; i < len(q.pending); {
		if now.Sub(q.pending[i].queued) >= q.queueAgeLimit() {
			q.remove(i)
			q.stats.DroppedExpired++
		} else {
			i++
		}
	}
}

func (q *streamIPSendQueue) queueAgeLimit() time.Duration {
	switch {
	case q.writeRateBytesPerSec >= 50*1024:
		return streamIPFastQueueAge
	case q.writeRateBytesPerSec >= 16*1024:
		return streamIPMidQueueAge
	default:
		return streamIPSlowQueueAge
	}
}

// Write completion is only a proxy for RF drain because the CoC socket can
// buffer data. The small socket send buffer forces backpressure early. A slow
// sample reduces the age limit immediately; faster samples rise gradually.
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
		q.writeRateBytesPerSec = (3*q.writeRateBytesPerSec + rate) / 4
	}
	q.expire(time.Now())
	for q.stats.Bytes > q.byteBudget() || q.fastBytes() > q.fastByteBudget() {
		victim := 0
		if q.fastBytes() > q.fastByteBudget() {
			for i, p := range q.pending {
				if p.fast {
					victim = i
					break
				}
			}
		} else {
			for i, p := range q.pending {
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
