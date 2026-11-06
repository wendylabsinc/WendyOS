//go:build linux

package localmesh

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// Every sender shares Wendy's ULA /48; the low UUID bits distinguish them.
func queueTestSource(source byte) [16]byte {
	return [16]byte{0xfd, 0x45, 0xb8, 0x91, 0x56, 0x7f, 0, 0, 0, 0, 0, 0, 0, 0, 0, source}
}
func queueTestIP(size int, source byte) []byte {
	p := make([]byte, size)
	p[0] = 0x60
	binary.BigEndian.PutUint16(p[4:6], uint16(size-40))
	p[6], p[7] = 58, 64
	src := queueTestSource(source)
	copy(p[8:24], src[:])
	return p
}
func queueTestAppUDP(size int, source byte, srcPort, dstPort uint16) []byte {
	p := queueTestIP(size, source)
	p[6] = 17
	binary.BigEndian.PutUint16(p[40:42], srcPort)
	binary.BigEndian.PutUint16(p[42:44], dstPort)
	binary.BigEndian.PutUint16(p[44:46], uint16(size-40))
	return p
}

func TestStreamIPQueueProtectsAppSetupFromSameSourceFlood(t *testing.T) {
	q := newStreamIPSendQueue()
	q.recordWrite(TunnelMTU, 90*time.Millisecond) // bounded source share at a slow rate
	now := time.Now()
	app := queueTestAppUDP(1230, 1, 49200, AppSessionPort)
	if !q.enqueue(1, app, now) || !q.enqueue(2, queueTestIP(1228, 2), now) {
		t.Fatal("initial app and bulk packets were not admitted")
	}
	for i := 0; i < 50; i++ {
		q.enqueue(uint32(3+i), queueTestIP(1228, 1), now)
	}
	served := 0
	first, ok := q.pop(now.Add(10*time.Millisecond), &served)
	if !ok || first.sequence != 1 || !first.fast {
		t.Fatalf("same-source flood displaced app QUIC setup: first=%+v ok=%t stats=%+v", first, ok, q.snapshot())
	}
	second, ok := q.pop(now.Add(10*time.Millisecond), &served)
	if !ok || second.sequence != 2 {
		t.Fatalf("app priority displaced the reserved other-source bulk packet: second=%+v ok=%t", second, ok)
	}
	if stats := q.snapshot(); stats.ByteLimit != streamIPMinByteBudget || stats.Bytes > stats.ByteLimit || stats.Packets != 3 || stats.DroppedCapacity == 0 {
		t.Fatalf("queue bounds or overload accounting changed: %+v", stats)
	}
}

func TestStreamIPQueueKeepsAppFlightAndBothSendersACKs(t *testing.T) {
	q := newStreamIPSendQueue()
	now := time.Now()
	packets := [][]byte{
		queueTestAppUDP(TunnelMTU, 1, 49200, AppSessionPort),
		queueTestAppUDP(80, 1, 49200, AppSessionPort),
		queueTestIP(TunnelMTU, 2),
		queueTestIP(80, 2),
	}
	for i, packet := range packets {
		if !q.enqueue(uint32(i+1), packet, now) {
			t.Fatalf("packet %d not admitted", i+1)
		}
	}
	served := 0
	seen := make(map[uint32]bool)
	for {
		packet, ok := q.pop(now, &served)
		if !ok {
			break
		}
		seen[packet.sequence] = true
	}
	for i := range packets {
		if !seen[uint32(i+1)] {
			t.Fatalf("an ACK displaced a bounded sender's waiting packet: missing=%d seen=%v", i+1, seen)
		}
	}
}

func TestStreamIPQueueAppSetupDisplacesOwnBulkAndPrioritizesResponse(t *testing.T) {
	q := newStreamIPSendQueue()
	q.recordWrite(TunnelMTU, 90*time.Millisecond)
	now := time.Now()
	q.enqueue(1, queueTestIP(1228, 1), now)
	q.enqueue(2, queueTestIP(1228, 1), now)
	q.enqueue(5, queueTestIP(1228, 1), now)
	q.enqueue(6, queueTestIP(1228, 1), now)
	q.enqueue(4, queueTestIP(1228, 2), now)
	if !q.enqueue(3, queueTestAppUDP(1230, 1, 49200, AppSessionPort), now) {
		t.Fatal("full-size app Initial was rejected behind same-source ICMP")
	}
	served := 0
	first, ok := q.pop(now, &served)
	if !ok || first.sequence != 3 || !first.fast {
		t.Fatalf("app Initial did not preempt own ICMP: %+v", first)
	}
	if !streamFastRoutedIP(queueTestAppUDP(624, 3, AppSessionPort, 49200)) {
		t.Fatal("server QUIC reply was not prioritized")
	}
	if stats := q.snapshot(); stats.Bytes > stats.ByteLimit || stats.Packets != 4 || stats.DroppedCapacity != 1 {
		t.Fatalf("app admission exceeded bounded queue: %+v", stats)
	}
}

func TestStreamFastRoutedIPRequiresCompleteUnfragmentedUDP(t *testing.T) {
	packet := queueTestAppUDP(1230, 1, 49200, AppSessionPort)
	if !streamFastRoutedIP(packet) {
		t.Fatal("valid full-size app QUIC packet not classified fast")
	}
	fragment := append([]byte(nil), packet...)
	fragment[6] = 44 // IPv6 Fragment extension header.
	if streamFastRoutedIP(fragment) {
		t.Fatal("fragmented packet used app priority")
	}
	invalidLength := append([]byte(nil), packet...)
	binary.BigEndian.PutUint16(invalidLength[44:46], 1200)
	if streamFastRoutedIP(invalidLength) {
		t.Fatal("invalid UDP length used app priority")
	}
	other := queueTestAppUDP(1230, 1, 49200, AppSessionPort+1)
	if streamFastRoutedIP(other) {
		t.Fatal("unrelated full-size UDP used app priority")
	}
}

func TestStreamIPQueueBoundsBytesAndSources(t *testing.T) {
	q := newStreamIPSendQueue()
	now := time.Now()
	for i := 0; i < 20; i++ {
		q.enqueue(uint32(i), queueTestIP(1200, 1), now)
	}
	if q.stats.Bytes > streamIPSourceByteBudget || q.stats.Packets > 4 {
		t.Fatalf("one sender monopolized the queue: %+v", q.snapshot())
	}
	for source := byte(2); source <= 4; source++ {
		for i := 0; i < 3; i++ {
			q.enqueue(uint32(source)*100+uint32(i), queueTestIP(1200, source), now)
		}
	}
	stats := q.snapshot()
	if stats.Bytes > stats.ByteLimit || stats.Packets != 9 || stats.DroppedCapacity == 0 {
		t.Fatalf("byte cap/overload counters: %+v", stats)
	}
	if q.sourceBytes(queueTestSource(1)) == 0 || q.sourceBytes(queueTestSource(2)) == 0 {
		t.Fatal("later senders evicted admitted packets")
	}
	served := 0
	if _, ok := q.pop(now, &served); !ok {
		t.Fatal("queued source could not drain")
	}
	if !q.enqueue(400, queueTestIP(1200, 4), now) {
		t.Fatal("new sender could not use released capacity")
	}
	for _, p := range q.pending {
		if q.sourceBytes(p.source) > stats.ByteLimit/2 {
			t.Fatalf("source %x exceeded budget: %+v", p.source, stats)
		}
	}
}

func TestStreamIPQueueExpiresBeforeWritingAndCountsDrops(t *testing.T) {
	q := newStreamIPSendQueue()
	now := time.Now()
	q.enqueue(1, queueTestIP(1200, 1), now)
	q.enqueue(2, queueTestIP(100, 2), now)
	served := 0
	if _, ok := q.pop(now.Add(streamIPMaxQueueAge), &served); ok {
		t.Fatal("expired packet escaped to L2CAP writer")
	}
	stats := q.snapshot()
	if stats.DroppedExpired != 2 || stats.Packets != 0 || stats.Bytes != 0 {
		t.Fatalf("expiry accounting: %+v", stats)
	}
}

func TestNodeSnapshotReadsLiveBLEQueueCounters(t *testing.T) {
	q := newStreamIPSendQueue()
	n := &Node{}
	n.view.Store(&NodeSnapshot{Links: []PeerLink{{Device: testDevice(t, testPrincipal(testTenantUUID, testDeviceAID)), queue: q}}})
	q.enqueue(1, queueTestIP(1200, 1), time.Now())
	s := n.Snapshot()
	if len(s.Links) != 1 || s.Links[0].SendQueue == nil || s.Links[0].SendQueue.Bytes != streamIPFramedBytes(1200) {
		t.Fatalf("BLE queue missing from snapshot: %+v", s.Links)
	}
	for i := uint32(2); i <= 8; i++ {
		q.enqueue(i, queueTestIP(1200, 1), time.Now())
	}
	second := n.Snapshot()
	if second.Links[0].SendQueue.DroppedCapacity == 0 || s.Links[0].SendQueue.DroppedCapacity != 0 {
		t.Fatalf("BLE queue snapshot did not update independently: first=%+v second=%+v", s.Links[0].SendQueue, second.Links[0].SendQueue)
	}
}

func TestStreamIPQueueAlternatesSourcesAndBoundsFastBurst(t *testing.T) {
	q := newStreamIPSendQueue()
	now := time.Now()
	for i := 0; i < 3; i++ {
		q.enqueue(uint32(10+i), queueTestIP(100, 1), now)
		q.enqueue(uint32(20+i), queueTestIP(100, 2), now)
	}
	q.enqueue(30, queueTestIP(1200, 3), now)
	served := 0
	want := []uint32{10, 20, 11, 21, 30, 12, 22}
	for i, expected := range want {
		got, ok := q.pop(now, &served)
		if !ok || got.sequence != expected {
			t.Fatalf("pop %d: got %d, %v; want %d", i, got.sequence, ok, expected)
		}
	}
	if stats := q.snapshot(); stats.Dequeued != uint64(len(want)) || stats.Bytes != 0 {
		t.Fatalf("drain accounting: %+v", stats)
	}
}

func TestStreamIPQueueKeepsTwoFullSizeSourcesAtSlowRate(t *testing.T) {
	q := newStreamIPSendQueue()
	q.recordWrite(TunnelMTU, 90*time.Millisecond) // 14 KiB/s Pi-class link.
	if got := q.snapshot().ByteLimit; got != streamIPMinByteBudget {
		t.Fatalf("slow-link byte budget = %d, want bounded room for new flights and existing replies", got)
	}
	now := time.Now()
	served := 0
	for round := 0; round < 2; round++ {
		// In the second round the previous dequeue was from A, so the
		// scheduler must still select B even though A arrived first.
		if round == 0 {
			q.enqueue(100, queueTestIP(TunnelMTU, 2), now)
		}
		q.enqueue(uint32(200+round), queueTestIP(TunnelMTU, 1), now)
		if round == 1 {
			q.enqueue(101, queueTestIP(TunnelMTU, 2), now)
		}
		for i := 0; i < 32; i++ {
			q.enqueue(uint32(300+round*32+i), queueTestIP(TunnelMTU, 1), now)
		}
		stats := q.snapshot()
		if stats.Bytes > stats.ByteLimit || stats.Packets != 5 || q.sourceBytes(queueTestSource(2)) == 0 {
			t.Fatalf("round %d: A flood evicted B or exceeded byte cap: %+v", round, stats)
		}
		for _, source := range [][16]byte{queueTestSource(2), queueTestSource(1)} {
			packet, ok := q.pop(now.Add(20*time.Millisecond), &served)
			if !ok || packet.source != source {
				t.Fatalf("round %d: dequeued source %x, ok=%t; want %x", round, packet.source, ok, source)
			}
		}
		// A can use four full-packet slots, but B's first packet must still
		// run before A's later packets. Drain those before the next round.
		for range 3 {
			packet, ok := q.pop(now.Add(20*time.Millisecond), &served)
			if !ok || packet.source != queueTestSource(1) {
				t.Fatalf("later A packet lost: %+v ok=%t", packet, ok)
			}
		}
	}
	if stats := q.snapshot(); stats.AgeLimitMS != 2000 || stats.MaxDequeuedWaitMS > 20 || stats.Bytes != 0 {
		t.Fatalf("slow-link sojourn/byte bound lost: %+v", stats)
	}
}

func TestStreamLinkSendsWaitingSourceThroughFullSizeFlood(t *testing.T) {
	a, b := net.Pipe()
	gate := &firstWriteGate{Conn: a, entered: make(chan struct{}), release: make(chan struct{})}
	link := newStreamNodeLink(gate)
	defer link.Close()
	defer b.Close()
	defer func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	}()
	link.ipPackets.recordWrite(TunnelMTU, 75*time.Millisecond)
	if err := link.SendIP(1, queueTestIP(TunnelMTU, 9)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("first packet did not reach writer")
	}
	// Both sources share this one outgoing stream. Keep the first packet
	// active while A overwhelms admission; B must survive until dequeue.
	if err := link.SendIP(2, queueTestIP(TunnelMTU, 2)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		if err := link.SendIP(uint32(3+i), queueTestIP(TunnelMTU, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if stats := link.ipPackets.snapshot(); stats.Packets != 5 || stats.Bytes > stats.ByteLimit || link.ipPackets.sourceBytes(queueTestSource(2)) == 0 {
		t.Fatalf("waiting source lost before write: %+v", stats)
	}
	close(gate.release)
	var assembler IPAssembler
	for i := 0; i < 4; i++ {
		kind, fragment := readStreamTestFrame(t, b)
		if kind != streamPacket {
			t.Fatalf("frame %d kind = %d", i, kind)
		}
		if id, want := binary.BigEndian.Uint32(fragment[1:5]), uint32(1+i/2); id != want {
			t.Fatalf("frame %d packet ID = %d, want %d", i, id, want)
		}
		packet, err := assembler.Receive(fragment, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if i == 3 && !bytes.Equal(packet, queueTestIP(TunnelMTU, 2)) {
			t.Fatal("waiting sender's packet was not delivered intact")
		}
	}
}

func TestStreamIPQueueAdaptsAgeToObservedWriteRate(t *testing.T) {
	q := newStreamIPSendQueue()
	if got := q.snapshot().AgeLimitMS; got != uint64(streamIPInitialQueueAge.Milliseconds()) {
		t.Fatalf("initial age limit = %d ms", got)
	}
	q.recordWrite(1280, 50*time.Millisecond)
	if got := q.snapshot().AgeLimitMS; got != 2000 {
		t.Fatalf("fast age limit = %d ms", got)
	}
	q.recordWrite(1280, 400*time.Millisecond)
	if got := q.snapshot().AgeLimitMS; got < 4100 || got > 4200 {
		t.Fatalf("slow rate did not allow the bounded queue to drain: %d ms", got)
	}
}

func TestStreamIPQueueHoldsSixPacketsAtFastBLEWriteRate(t *testing.T) {
	q := newStreamIPSendQueue()
	for i := 0; i < 10; i++ {
		q.recordWrite(1280, 10*time.Millisecond)
	}
	now := time.Now()
	for i := 0; i < 6; i++ {
		q.enqueue(uint32(i+1), queueTestIP(TunnelMTU, byte(i%2+1)), now)
	}
	if stats := q.snapshot(); stats.Packets != 6 || stats.Bytes > streamIPByteBudget {
		t.Fatalf("fast BLE queue did not hold six packets: %+v", stats)
	}
	served := 0
	for i := 0; i < 6; i++ {
		if _, ok := q.pop(now.Add(150*time.Millisecond), &served); !ok {
			t.Fatalf("fast packet %d expired within 200 ms age budget", i)
		}
	}
}

func TestStreamIPQueueShrinksImmediatelyAfterSlowWrite(t *testing.T) {
	q := newStreamIPSendQueue()
	for i := 0; i < 10; i++ {
		q.recordWrite(1280, 10*time.Millisecond)
	}
	now := time.Now()
	for i := 0; i < 12; i++ {
		q.enqueue(uint32(i+1), queueTestIP(TunnelMTU, byte(i%2+1)), now)
	}
	q.recordWrite(1280, 400*time.Millisecond)
	stats := q.snapshot()
	if stats.Bytes > stats.ByteLimit || stats.ByteLimit != streamIPMinByteBudget || stats.DroppedCapacity == 0 {
		t.Fatalf("slow link did not shed queued full packets: %+v", stats)
	}
}

func TestStreamIPQueueHoldsSixShortPacketsAt25KiBRate(t *testing.T) {
	q := newStreamIPSendQueue()
	for i := 0; i < 10; i++ {
		q.recordWrite(1280, 50*time.Millisecond)
	}
	now := time.Now()
	for i := 0; i < 6; i++ {
		q.enqueue(uint32(i+1), queueTestIP(256, byte(i%2+1)), now)
	}
	if stats := q.snapshot(); stats.Packets != 6 || stats.ByteLimit >= streamIPByteBudget {
		t.Fatalf("25 KiB/s queue did not retain six short packets within dynamic cap: %+v", stats)
	}
}

func TestStreamIPQueueReservesBulkCapacityDuringFastFlood(t *testing.T) {
	q := newStreamIPSendQueue()
	now := time.Now()
	for i := 0; i < 100; i++ {
		q.enqueue(uint32(i), queueTestIP(100, byte(i%5+1)), now)
	}
	if q.fastBytes() > q.fastByteBudget() {
		t.Fatalf("short packets consumed bulk reservation: %+v", q.snapshot())
	}
	q.enqueue(1000, queueTestIP(TunnelMTU, 9), now)
	if q.sourceBytes(queueTestSource(9)) == 0 {
		t.Fatal("bulk packet starved by short-packet flood")
	}
}

func TestStreamIPQueueDeliversWaitingFlightsAfterSlowWrite(t *testing.T) {
	for _, measured := range []bool{false, true} {
		t.Run(fmt.Sprintf("measured-%t", measured), func(t *testing.T) {
			a, b := net.Pipe()
			gate := &firstWriteGate{Conn: a, entered: make(chan struct{}), release: make(chan struct{})}
			link := newStreamNodeLink(gate)
			defer link.Close()
			defer b.Close()
			defer func() {
				select {
				case <-gate.release:
				default:
					close(gate.release)
				}
			}()
			if measured {
				link.ipPackets.recordWrite(TunnelMTU, 400*time.Millisecond)
			}
			packets := [][]byte{queueTestIP(TunnelMTU, 9), queueTestAppUDP(1248, 1, 49200, AppSessionPort), queueTestIP(TunnelMTU, 2)}
			if err := link.SendIP(1, packets[0]); err != nil {
				t.Fatal(err)
			}
			select {
			case <-gate.entered:
			case <-time.After(time.Second):
				t.Fatal("first packet did not reach writer")
			}
			for i := 1; i < len(packets); i++ {
				if err := link.SendIP(uint32(i+1), packets[i]); err != nil {
					t.Fatal(err)
				}
			}
			// Physical Pi writes take hundreds of milliseconds. Neither waiting
			// sender has had a transmission opportunity while this write is held.
			time.Sleep(300 * time.Millisecond)
			close(gate.release)
			var assembler IPAssembler
			seen := make(map[uint32]bool)
			for len(seen) < len(packets) {
				kind, frame := readStreamTestFrame(t, b)
				if kind != streamPacket {
					t.Fatalf("wire kind = %d", kind)
				}
				packet, err := assembler.Receive(frame, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				if packet != nil {
					id := binary.BigEndian.Uint32(frame[1:5])
					if id < 1 || id > 3 || seen[id] || !bytes.Equal(packet, packets[id-1]) {
						t.Fatalf("packet lost, duplicated or changed: id=%d", id)
					}
					seen[id] = true
				}
			}
			if stats := link.ipPackets.snapshot(); stats.DroppedExpired != 0 || stats.DroppedCapacity != 0 || stats.Bytes != 0 {
				t.Fatalf("uncontended bounded flights were discarded: %+v", stats)
			}
		})
	}
}

func TestStreamIPQueueNeverEmitsAnOrphanFragment(t *testing.T) {
	a, b := net.Pipe()
	gate := &firstWriteGate{Conn: a, entered: make(chan struct{}), release: make(chan struct{})}
	link := newStreamNodeLink(gate)
	defer link.Close()
	defer b.Close()
	first := queueTestIP(1200, 1)
	if err := link.SendIP(1, first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("first packet did not reach writer")
	}
	for i := 0; i < 20; i++ {
		if err := link.SendIP(uint32(10+i), queueTestIP(1200, 2)); err != nil {
			t.Fatal(err)
		}
	}
	// The first packet has begun and must finish intact. Make every waiting
	// packet older than even the maximum age, without delaying the active
	// write or depending on how quickly the machine schedules it.
	link.ipPackets.mu.Lock()
	for i := range link.ipPackets.pending {
		link.ipPackets.pending[i].queued = time.Now().Add(-streamIPMaxQueueAge)
	}
	link.ipPackets.mu.Unlock()
	close(gate.release)
	var assembler IPAssembler
	for i := 0; i < 2; i++ {
		kind, frame := readStreamTestFrame(t, b)
		if kind != streamPacket {
			t.Fatalf("wire kind = %d", kind)
		}
		got, err := assembler.Receive(frame, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 && !bytes.Equal(got, first) {
			t.Fatalf("packet split or changed: %x", got)
		}
	}
	if err := b.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var extra [1]byte
	if _, err := b.Read(extra[:]); err == nil {
		t.Fatal("expired packet reached wire")
	} else {
		var e net.Error
		if !errors.As(err, &e) || !e.Timeout() {
			t.Fatalf("unexpected stream read result: %v", err)
		}
	}
	stats := link.ipPackets.snapshot()
	if stats.DroppedExpired == 0 || stats.Dequeued != 1 {
		t.Fatalf("packet drop was not accounted before write: %+v", stats)
	}
}

func TestStreamIPQueueKeepsFragmentsAdjacentBeforeNextPacket(t *testing.T) {
	a, b := net.Pipe()
	link := newStreamNodeLink(a)
	defer link.Close()
	defer b.Close()
	for i := 0; i < 10; i++ {
		link.ipPackets.recordWrite(1280, 10*time.Millisecond)
	}
	first, second := queueTestIP(1200, 1), queueTestIP(1200, 2)
	if err := link.SendIP(101, first); err != nil {
		t.Fatal(err)
	}
	if err := link.SendIP(102, second); err != nil {
		t.Fatal(err)
	}
	var assembler IPAssembler
	for i := 0; i < 4; i++ {
		kind, d := readStreamTestFrame(t, b)
		if kind != streamPacket {
			t.Fatalf("frame %d kind %d", i, kind)
		}
		id := binary.BigEndian.Uint32(d[1:5])
		if want := uint32(101 + i/2); id != want {
			t.Fatalf("frame %d packet %d, want %d", i, id, want)
		}
		p, err := assembler.Receive(d, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 1 && !bytes.Equal(p, [][]byte{first, second}[i/2]) {
			t.Fatalf("reassembled packet %d differs", i/2)
		}
	}
}

func TestStreamIPQueueKeepsBabelAheadOfWaitingRoutedPacket(t *testing.T) {
	a, b := net.Pipe()
	gate := &firstWriteGate{Conn: a, entered: make(chan struct{}), release: make(chan struct{})}
	link := newStreamNodeLink(gate)
	defer link.Close()
	defer b.Close()
	if err := link.SendIP(1, queueTestIP(1200, 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("first routed packet did not start")
	}
	if err := link.SendIP(2, queueTestIP(1200, 2)); err != nil {
		t.Fatal(err)
	}
	if err := link.SendDatagram([]byte{PacketBabel, 7}); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	for i := 0; i < 5; i++ {
		_, d := readStreamTestFrame(t, b)
		if i == 2 {
			if !bytes.Equal(d, []byte{PacketBabel, 7}) {
				t.Fatalf("Babel was blocked by queued IP: frame %d = %x", i, d)
			}
			continue
		}
		wantID := uint32(1)
		if i > 2 {
			wantID = 2
		}
		if got := binary.BigEndian.Uint32(d[1:5]); got != wantID {
			t.Fatalf("fragment %d belonged to packet %d, want %d", i, got, wantID)
		}
	}
}

func TestStreamIPQueueDistinguishesUUIDSuffixSources(t *testing.T) {
	q := newStreamIPSendQueue()
	a, b := queueTestIP(TunnelMTU, 1), queueTestIP(TunnelMTU, 2)
	if !bytes.Equal(a[8:14], b[8:14]) || bytes.Equal(a[14:24], b[14:24]) {
		t.Fatal("fixture must share a ULA /48 but differ in final UUID bits")
	}
	now := time.Now()
	q.enqueue(1, b, now)
	for i := uint32(2); i < 32; i++ {
		q.enqueue(i, a, now)
	}
	if q.sourceBytes(queueTestSource(1)) == 0 || q.sourceBytes(queueTestSource(2)) == 0 {
		t.Fatal("IPv6 UUID suffixes merged into one sender budget")
	}
	served := 0
	first, ok := q.pop(now, &served)
	if !ok || first.sequence != 1 {
		t.Fatal("same-prefix sender starved by flood")
	}
	second, ok := q.pop(now, &served)
	if !ok || second.source != queueTestSource(1) {
		t.Fatal("other IPv6 source not served next")
	}
}

func TestStreamFastRoutedIPv6Bounds(t *testing.T) {
	for _, packet := range [][]byte{nil, {0x60}, make([]byte, 40)} {
		if streamFastRoutedIP(packet) {
			t.Fatal("malformed packet classified fast")
		}
		if newStreamIPSendQueue().enqueue(1, packet, time.Now()) {
			t.Fatal("malformed packet admitted")
		}
	}
	for _, next := range []byte{0, 43, 44, 60} {
		p := queueTestAppUDP(1200, 1, 49200, AppSessionPort)
		p[6] = next
		if streamFastRoutedIP(p) {
			t.Fatalf("extension header %d interpreted as UDP", next)
		}
	}
	p := queueTestAppUDP(1200, 1, 49200, AppSessionPort)
	binary.BigEndian.PutUint16(p[4:6], 1100)
	if streamFastRoutedIP(p) {
		t.Fatal("incomplete IPv6 payload got UDP priority")
	}
}

// A QUIC ClientHello spans multiple datagrams, and a certificate flight is a
// larger burst. Before its first bytes arrive, later bytes cannot advance TLS.
// With the writer occupied, overload must preserve the admitted flight prefix.
func TestStreamIPQueueKeepsAdmittedFlightPrefix(t *testing.T) {
	for _, rate := range []string{"startup", "slow", "fast"} {
		for _, count := range []int{2, 13} {
			t.Run(fmt.Sprintf("%s/%d_datagrams", rate, count), func(t *testing.T) {
				q := newStreamIPSendQueue()
				if rate == "slow" {
					q.recordWrite(TunnelMTU, 90*time.Millisecond)
				}
				if rate == "fast" {
					for range 10 {
						q.recordWrite(TunnelMTU, 10*time.Millisecond)
					}
				}
				now := time.Now()
				packet := queueTestAppUDP(1248, 1, 49200, AppSessionPort)
				for i := 1; i <= count; i++ {
					q.enqueue(uint32(i), packet, now)
				}
				served := 0
				for want := uint32(1); ; want++ {
					got, ok := q.pop(now, &served)
					if !ok {
						if want == 1 {
							t.Fatal("no admitted datagram made progress")
						}
						break
					}
					if got.sequence != want {
						t.Fatalf("flight prefix lost before writer had a turn: got datagram %d, want %d; stats=%+v", got.sequence, want, q.snapshot())
					}
				}
			})
		}
	}
}

func TestStreamIPQueueRateReductionKeepsAdmittedPrefixes(t *testing.T) {
	q := newStreamIPSendQueue()
	for range 10 {
		q.recordWrite(TunnelMTU, 10*time.Millisecond)
	}
	now := time.Now()
	for i := 0; i < 12; i++ {
		q.enqueue(uint32(i+1), queueTestIP(TunnelMTU, byte(i%2+1)), now)
	}
	q.recordWrite(TunnelMTU, 400*time.Millisecond)
	served := 0
	for want := uint32(1); want <= 8; want++ {
		p, ok := q.pop(now, &served)
		if !ok || p.sequence != want {
			t.Fatalf("rate reduction discarded a sender's admitted prefix: got %d ok=%t want=%d", p.sequence, ok, want)
		}
	}
	if stats := q.snapshot(); stats.Bytes != 0 || stats.DroppedCapacity != 4 {
		t.Fatalf("rate reduction did not retain bounded first packets: %+v", stats)
	}
}

func TestStreamIPQueueRejectedFlightDoesNotDiscardOwnBulk(t *testing.T) {
	q := newStreamIPSendQueue()
	now := time.Now()
	for seq := uint32(1); seq <= 4; seq++ {
		if !q.enqueue(seq, queueTestAppUDP(1248, 1, 49200, AppSessionPort), now) {
			t.Fatal("fixture does not fit bounded source share")
		}
	}
	if !q.enqueue(5, queueTestIP(300, 1), now) {
		t.Fatal("fixture does not fit bounded source share")
	}
	if q.enqueue(6, queueTestAppUDP(1248, 1, 49200, AppSessionPort), now) {
		t.Fatal("fifth full flight exceeded its source share")
	}
	served := 0
	for _, want := range []uint32{1, 2, 3, 4, 5} {
		p, ok := q.pop(now, &served)
		if !ok || p.sequence != want {
			t.Fatalf("rejected packet discarded admitted traffic: got %d ok=%t want=%d", p.sequence, ok, want)
		}
	}
}

// The physical three-hop failure repeatedly delivered the first of two QUIC
// Initial datagrams, losing the same ClientHello suffix on every PTO retry.
// Both must fit before the writer gets a turn, including at startup/slow rates.
func TestStreamIPQueueAdmitsCompleteTwoInitialFlight(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(fmt.Sprintf("slow=%t", slow), func(t *testing.T) {
			q := newStreamIPSendQueue()
			if slow {
				q.recordWrite(TunnelMTU, 400*time.Millisecond)
			}
			now := time.Now()
			for _, seq := range []uint32{1, 2} {
				if !q.enqueue(seq, queueTestAppUDP(1248, 1, 49200, AppSessionPort), now) {
					t.Fatalf("ClientHello datagram %d rejected before writer ran: %+v", seq, q.snapshot())
				}
			}
			if !q.enqueue(3, queueTestAppUDP(80, 1, 49200, AppSessionPort), now) || !q.enqueue(4, queueTestIP(TunnelMTU, 2), now) {
				t.Fatal("flight consumed ACK or other-sender reservation")
			}
			served := 0
			for _, want := range []uint32{1, 2, 3, 4} {
				got, ok := q.pop(now, &served)
				if !ok || got.sequence != want {
					t.Fatalf("admitted prefix changed: got %d ok=%t want=%d", got.sequence, ok, want)
				}
			}
			if got := q.snapshot(); got.Bytes != 0 || got.DroppedCapacity != 0 {
				t.Fatalf("bad queue accounting: %+v", got)
			}
		})
	}
}

// AV hardware lost every ClientHello suffix at an intermediate BLE hop while
// small packets from earlier app flows occupied the same source's share. A
// quiet-source two-Initial test misses that case: source identity is a device,
// so a new flow competes with the device's existing ACKs and replies.
func TestStreamIPQueueNewInitialFlightBehindExistingFlowReplies(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(fmt.Sprintf("slow=%t", slow), func(t *testing.T) {
			q := newStreamIPSendQueue()
			if slow {
				q.recordWrite(TunnelMTU, 400*time.Millisecond)
			}
			now := time.Now()
			var admitted []uint32
			for source := byte(1); source <= 2; source++ {
				for i := range 8 {
					seq := uint32(source)*100 + uint32(i)
					if !q.enqueue(seq, queueTestAppUDP(100, source, 49100+uint16(i), AppSessionPort), now) {
						t.Fatal("small existing-flow backlog rejected")
					}
					admitted = append(admitted, seq)
				}
				for i := range 2 {
					seq := uint32(source)*100 + 10 + uint32(i)
					if !q.enqueue(seq, queueTestAppUDP(1248, source, 49200, AppSessionPort), now) {
						t.Fatalf("new source%d ClientHello packet%d lost behind existing replies: %+v", source, i, q.snapshot())
					}
					admitted = append(admitted, seq)
				}
			}
			served := 0
			seen := make(map[uint32]bool)
			for range admitted {
				packet, ok := q.pop(now, &served)
				if !ok || seen[packet.sequence] {
					t.Fatal("admitted packet lost or duplicated")
				}
				seen[packet.sequence] = true
			}
			for _, seq := range admitted {
				if !seen[seq] {
					t.Fatalf("admitted packet %d displaced", seq)
				}
			}
			if stats := q.snapshot(); stats.Bytes != 0 || stats.DroppedCapacity != 0 || stats.DroppedExpired != 0 {
				t.Fatalf("bounded mixed-flow admission lost packets: %+v", stats)
			}
		})
	}
}

func TestStreamIPQueueRetainsNextSenderDuringSlowPacketWrite(t *testing.T) {
	q := newStreamIPSendQueue()
	q.recordWrite(TunnelMTU, 700*time.Millisecond)
	now := time.Now()
	if !q.enqueue(1, queueTestIP(TunnelMTU, 1), now) || !q.enqueue(2, queueTestIP(TunnelMTU, 2), now) {
		t.Fatal("queue refused bounded competing senders")
	}
	served := 0
	first, ok := q.pop(now, &served)
	if !ok {
		t.Fatal("first sender missing")
	}
	second, ok := q.pop(now.Add(700*time.Millisecond), &served)
	if !ok || second.source == first.source {
		t.Fatal("waiting sender expired during one BLE packet write")
	}
}

func TestStreamIPQueueBoundsAgeForStalledCarrier(t *testing.T) {
	q := newStreamIPSendQueue()
	q.recordWrite(TunnelMTU, 10*time.Second)
	now := time.Now()
	q.enqueue(1, queueTestIP(TunnelMTU, 1), now)
	served := 0
	if _, ok := q.pop(now.Add(5*time.Second), &served); ok {
		t.Fatal("stalled carrier retained stale IP traffic beyond the bound")
	}
}

// A TCP flight arrives at the router much faster than BLE can serialize it.
// Replacing its oldest queued segment with each later segment creates a hole
// at the receiver and turns every flight into an RTO-driven retransmission.
func queueTestTCP(size int, source byte, sequence uint32) []byte {
	p := queueTestIP(size, source)
	p[6] = 6
	binary.BigEndian.PutUint16(p[40:42], 18867)
	binary.BigEndian.PutUint16(p[42:44], 60000)
	binary.BigEndian.PutUint32(p[44:48], sequence)
	p[52] = 5 << 4
	p[53] = 0x18
	return p
}

func TestStreamIPQueueKeepsTCPFlightPrefix(t *testing.T) {
	q := newStreamIPSendQueue()
	now := time.Now()
	q.enqueue(1, queueTestTCP(1320, 1, 1000), now)
	q.enqueue(2, queueTestIP(1320, 2), now)
	for i := uint32(1); i < 5; i++ {
		q.enqueue(i+2, queueTestTCP(1320, 1, 1000+i*1260), now)
	}
	served := 0
	first, ok := q.pop(now, &served)
	if !ok || first.sequence != 1 {
		t.Fatalf("TCP flight lost its first waiting segment: id=%d ok=%v", first.sequence, ok)
	}
	second, ok := q.pop(now, &served)
	if !ok || second.sequence != 2 {
		t.Fatal("TCP flight displaced another sender")
	}
}

func TestStreamIPQueueAllowsEarlierTCPRetransmission(t *testing.T) {
	q := newStreamIPSendQueue()
	now := time.Now()
	q.enqueue(1, queueTestTCP(1320, 1, 2260), now)
	if !q.enqueue(2, queueTestTCP(1320, 1, 1000), now) {
		t.Fatal("lost earlier TCP retransmission")
	}
	served := 0
	p, ok := q.pop(now, &served)
	if !ok || p.sequence != 2 {
		t.Fatal("did not preserve the earlier retransmission")
	}
}

func TestTCPQueueOrderingIsScopedAndWrapSafe(t *testing.T) {
	first := queueTestTCP(1320, 1, ^uint32(0)-500)
	next := queueTestTCP(1320, 1, 759)
	if !tcpQueuedBefore(first, next) || tcpQueuedBefore(next, first) {
		t.Fatal("TCP sequence wrap reversed queue order")
	}
	different := append([]byte(nil), next...)
	different[43]++
	if tcpQueuedBefore(first, different) {
		t.Fatal("different TCP port shared ordering")
	}
	different = append([]byte(nil), next...)
	different[39]++
	if tcpQueuedBefore(first, different) {
		t.Fatal("different destination shared ordering")
	}
	ack := next[:60]
	if tcpQueuedBefore(first, ack) {
		t.Fatal("pure ACK was held behind queued data")
	}
	malformed := append([]byte(nil), next...)
	malformed[52] = 0
	if tcpQueuedBefore(first, malformed) {
		t.Fatal("invalid TCP header was classified")
	}
	extension := append([]byte(nil), next...)
	extension[6] = 44
	if tcpQueuedBefore(first, extension) {
		t.Fatal("fragment was classified as whole TCP")
	}
}

func TestTCPQueueOrderingTracksInnerLeaseFlow(t *testing.T) {
	wrap := func(sequence uint32) []byte {
		tcp := queueTestTCP(1280, 1, sequence)
		p := queueTestIP(1300, 1)
		p[6], p[40], p[49] = 4, 0x45, 6
		binary.BigEndian.PutUint16(p[42:44], 1260)
		copy(p[52:60], []byte{10, 88, 0, 2, 8, 8, 8, 8})
		copy(p[60:], tcp[40:])
		return p
	}
	first, next := wrap(1000), wrap(2260)
	if !tcpQueuedBefore(first, next) {
		t.Fatal("lease TCP lost ordering")
	}
	next[59]++
	if tcpQueuedBefore(first, next) {
		t.Fatal("different Internet destination shared ordering")
	}
	next = wrap(2260)
	next[46] = 0x20
	if tcpQueuedBefore(first, next) {
		t.Fatal("fragmented inner packet was classified")
	}
	next = wrap(2260)
	next[40] = 0x46
	if tcpQueuedBefore(first, next) {
		t.Fatal("IPv4 options were classified as base TCP")
	}
}

func TestStreamIPQueueBufferedACKDoesNotEraseSlowRate(t *testing.T) {
	q := newStreamIPSendQueue()
	q.recordWrite(1320, 700*time.Millisecond)
	now := time.Now()
	q.enqueue(1, queueTestTCP(1320, 1, 1000), now)
	q.recordWrite(60, time.Microsecond)
	served := 0
	if p, ok := q.pop(now.Add(700*time.Millisecond), &served); !ok || p.sequence != 1 {
		t.Fatal("instant buffered ACK expired the next slow full-size write")
	}
}

func TestStreamIPQueueKeepsDataWhileSendingTCPACKs(t *testing.T) {
	q := newStreamIPSendQueue()
	now := time.Now()
	for source := byte(1); source <= 2; source++ {
		q.enqueue(uint32(source), queueTestTCP(TunnelMTU, source, 1000), now)
		ack := queueTestTCP(60, source, 1000)
		ack[53] = 0x10
		q.enqueue(uint32(source)+10, ack, now)
	}
	served := 0
	seen := map[uint32]bool{}
	for {
		p, ok := q.pop(now, &served)
		if !ok {
			break
		}
		seen[p.sequence] = true
	}
	for _, id := range []uint32{1, 2, 11, 12} {
		if !seen[id] {
			t.Fatalf("two TCP senders could not retain their full packets and ACKs: missing=%d seen=%v", id, seen)
		}
	}
}

func TestStreamIPQueueEarlierTCPRetransmissionPreservesOtherFlows(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%t", full), func(t *testing.T) {
			q := newStreamIPSendQueue()
			now := time.Now()
			if !q.enqueue(1, queueTestTCP(1320, 1, 2260), now) || !q.enqueue(2, queueTestIP(1320, 2), now) {
				t.Fatal("fixture not admitted")
			}
			if full {
				for i, id := range []uint32{3, 5, 6} {
					if !q.enqueue(id, queueTestTCP(1320, 1, 3520+uint32(i)*1260), now) {
						t.Fatal("later segment not admitted before source share filled")
					}
				}
			}
			if !q.enqueue(4, queueTestTCP(1320, 1, 1000), now) {
				t.Fatal("earlier retransmission lost")
			}
			served := 0
			// Service the missing TCP prefix first, then the other sender, then the
			// already admitted later segment. Capacity pressure may shed only its tail.
			wantIDs := []uint32{4, 2, 1}
			if full {
				wantIDs = append(wantIDs, 3, 5)
			}
			for _, want := range wantIDs {
				p, ok := q.pop(now, &served)
				if !ok || p.sequence != want {
					t.Fatalf("got id=%d ok=%t want=%d", p.sequence, ok, want)
				}
			}
			wantDrops := uint64(0)
			if full {
				wantDrops = 1
			}
			if got := q.snapshot(); got.Bytes != 0 || got.DroppedCapacity != wantDrops {
				t.Fatalf("accounting: %+v", got)
			}
		})
	}
}

func TestStreamIPQueueRefusedFastTCPPrefixKeepsBulkTail(t *testing.T) {
	q := newStreamIPSendQueue()
	now := time.Now()
	if !q.enqueue(1, queueTestTCP(TunnelMTU, 1, 2260), now) {
		t.Fatal("bulk TCP tail not admitted")
	}
	// Fill the independent fast-lane budget with other sources. A small
	// retransmission cannot free fast capacity by evicting its bulk tail.
	for source := byte(2); source < 250; source++ {
		if !q.enqueue(uint32(source), queueTestIP(streamFastIPMax, source), now) {
			break
		}
	}
	before := q.snapshot()
	if q.enqueue(1000, queueTestTCP(streamFastIPMax, 1, 1000), now) {
		t.Fatal("prefix exceeded the fast-lane budget")
	}
	after := q.snapshot()
	if after.Bytes != before.Bytes || after.Packets != before.Packets || after.DroppedCapacity != before.DroppedCapacity+1 {
		t.Fatalf("refused prefix discarded admitted traffic: before=%+v after=%+v", before, after)
	}
}

func TestStreamIPQueueRefusedLargerTCPPrefixKeepsTail(t *testing.T) {
	q := newStreamIPSendQueue()
	now := time.Now()
	if !q.enqueue(1, queueTestTCP(500, 1, 2260), now) || !q.enqueue(2, queueTestIP(TunnelMTU, 2), now) {
		t.Fatal("bulk packets not admitted")
	}
	for source := byte(3); q.snapshot().Bytes+streamIPFramedBytes(streamFastIPMax) <= streamIPMinByteBudget; source++ {
		if !q.enqueue(uint32(source), queueTestIP(streamFastIPMax, source), now) {
			t.Fatal("fixture fast packet not admitted")
		}
	}
	before := q.snapshot()
	if q.enqueue(1000, queueTestTCP(TunnelMTU, 1, 1000), now) {
		t.Fatal("prefix exceeded capacity available to its flow")
	}
	after := q.snapshot()
	if after.Bytes != before.Bytes || after.Packets != before.Packets || after.DroppedCapacity != before.DroppedCapacity+1 {
		t.Fatalf("refused larger prefix discarded admitted traffic: before=%+v after=%+v", before, after)
	}
}
