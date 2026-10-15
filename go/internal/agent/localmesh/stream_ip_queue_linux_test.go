//go:build linux

package localmesh

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"
)

func queueTestIP(size int, source byte) []byte {
	p := make([]byte, size)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], uint16(size))
	p[12] = source
	return p
}

func queueTestAppUDP(size int, source byte, srcPort, dstPort uint16) []byte {
	p := queueTestIP(size, source)
	p[9] = 17
	binary.BigEndian.PutUint16(p[20:22], srcPort)
	binary.BigEndian.PutUint16(p[22:24], dstPort)
	binary.BigEndian.PutUint16(p[24:26], uint16(size-20))
	return p
}

func TestStreamIPQueueProtectsAppSetupFromSameSourceFlood(t *testing.T) {
	q := newStreamIPSendQueue()
	q.recordWrite(TunnelMTU, 90*time.Millisecond) // two full-size waiting slots
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
	if stats := q.snapshot(); stats.ByteLimit != streamIPMinByteBudget || stats.Bytes != 0 || stats.DroppedCapacity == 0 {
		t.Fatalf("queue bounds or overload accounting changed: %+v", stats)
	}
}

func TestStreamIPQueueAppSetupDisplacesOwnBulkAndPrioritizesResponse(t *testing.T) {
	q := newStreamIPSendQueue()
	q.recordWrite(TunnelMTU, 90*time.Millisecond)
	now := time.Now()
	q.enqueue(1, queueTestIP(1228, 1), now)
	q.enqueue(2, queueTestIP(1228, 2), now)
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
	if stats := q.snapshot(); stats.Bytes > stats.ByteLimit || stats.Packets != 1 {
		t.Fatalf("app admission exceeded bounded queue: %+v", stats)
	}
}

func TestStreamFastRoutedIPRequiresCompleteUnfragmentedUDP(t *testing.T) {
	packet := queueTestAppUDP(1230, 1, 49200, AppSessionPort)
	if !streamFastRoutedIP(packet) {
		t.Fatal("valid full-size app QUIC packet not classified fast")
	}
	fragment := append([]byte(nil), packet...)
	binary.BigEndian.PutUint16(fragment[6:8], 0x2000)
	if streamFastRoutedIP(fragment) {
		t.Fatal("fragmented packet used app priority")
	}
	invalidLength := append([]byte(nil), packet...)
	binary.BigEndian.PutUint16(invalidLength[24:26], 1200)
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
	if q.stats.Bytes > streamIPSourceByteBudget || q.stats.Packets > 3 {
		t.Fatalf("one sender monopolized the queue: %+v", q.snapshot())
	}
	for source := byte(2); source <= 4; source++ {
		for i := 0; i < 3; i++ {
			q.enqueue(uint32(source)*100+uint32(i), queueTestIP(1200, source), now)
		}
	}
	stats := q.snapshot()
	if stats.Bytes > streamIPByteBudget || stats.Packets == 0 || stats.DroppedCapacity == 0 {
		t.Fatalf("byte cap/overload counters: %+v", stats)
	}
	if q.sourceBytes(4<<24) == 0 {
		t.Fatal("new sender was starved by older sender backlog")
	}
	for _, p := range q.pending {
		if q.sourceBytes(p.source) > streamIPSourceByteBudget {
			t.Fatalf("source %d exceeded budget: %+v", p.source, stats)
		}
	}
}

func TestStreamIPQueueExpiresBeforeWritingAndCountsDrops(t *testing.T) {
	q := newStreamIPSendQueue()
	now := time.Now()
	q.enqueue(1, queueTestIP(1200, 1), now)
	q.enqueue(2, queueTestIP(100, 2), now)
	served := 0
	if _, ok := q.pop(now.Add(streamIPSlowQueueAge), &served); ok {
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
	n.view.Store(&NodeSnapshot{Links: []PeerLink{{Asset: 358, queue: q}}})
	q.enqueue(1, queueTestIP(1200, 1), time.Now())
	s := n.Snapshot()
	if len(s.Links) != 1 || s.Links[0].SendQueue == nil || s.Links[0].SendQueue.Bytes != streamIPFramedBytes(1200) {
		t.Fatalf("BLE queue missing from snapshot: %+v", s.Links)
	}
	for i := uint32(2); i <= 4; i++ {
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
	if got := q.snapshot().ByteLimit; got != 2*streamIPFramedBytes(TunnelMTU) {
		t.Fatalf("slow-link byte budget = %d, want room for two full packets", got)
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
		if stats.Bytes > stats.ByteLimit || stats.Packets != 2 || q.sourceBytes(2<<24) == 0 {
			t.Fatalf("round %d: A flood evicted B or exceeded byte cap: %+v", round, stats)
		}
		for _, source := range []uint32{2 << 24, 1 << 24} {
			packet, ok := q.pop(now.Add(20*time.Millisecond), &served)
			if !ok || packet.source != source {
				t.Fatalf("round %d: dequeued source %d, ok=%t; want %d", round, packet.source, ok, source)
			}
		}
	}
	if stats := q.snapshot(); stats.AgeLimitMS != 50 || stats.MaxDequeuedWaitMS > 50 || stats.Bytes != 0 {
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
	if stats := link.ipPackets.snapshot(); stats.Packets != 2 || stats.Bytes > stats.ByteLimit || link.ipPackets.sourceBytes(2<<24) == 0 {
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
	if got := q.snapshot().AgeLimitMS; got != 50 {
		t.Fatalf("initial age limit = %d ms", got)
	}
	q.recordWrite(1280, 50*time.Millisecond)
	if got := q.snapshot().AgeLimitMS; got != 150 {
		t.Fatalf("fast age limit = %d ms", got)
	}
	q.recordWrite(1280, 400*time.Millisecond)
	if got := q.snapshot().AgeLimitMS; got != 50 {
		t.Fatalf("slow rate did not tighten queue age: %d ms", got)
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
	for i := 0; i < 6; i++ {
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
	if q.sourceBytes(9<<24) == 0 {
		t.Fatal("bulk packet starved by short-packet flood")
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
	// The first packet has begun and must finish intact. Every other packet
	// must expire whole before its first fragment enters the byte stream.
	time.Sleep(streamIPSlowQueueAge + 50*time.Millisecond)
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
