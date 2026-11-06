//go:build linux

package localmesh

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func TestStreamLinkBabelPassesWaitingMetadata(t *testing.T) {
	a, b := net.Pipe()
	gate := &firstWriteGate{Conn: a, entered: make(chan struct{}), release: make(chan struct{})}
	link := newStreamNodeLink(gate)
	t.Cleanup(func() { _ = link.Close(); _ = b.Close() })
	if err := link.SendDatagram([]byte{PacketIP, 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	// A catalogue/directory writer has queued its next frame while the current
	// IP packet drains. A later routing hello must get the next frame boundary;
	// putting it behind this metadata adds another whole slow serialization.
	metadata := streamFrame{kind: streamControl, data: bytes.Repeat([]byte{'m'}, 1024)}
	link.priority <- metadata
	hello := []byte{PacketBabel, 7}
	if err := link.SendDatagram(hello); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	want := []streamFrame{{kind: streamPacket, data: []byte{PacketIP, 1}}, {kind: streamPacket, data: hello}, metadata}
	for i, expected := range want {
		kind, got := readStreamTestFrame(t, b)
		if kind != expected.kind || !bytes.Equal(got, expected.data) {
			t.Fatalf("frame %d: kind=%d, bytes=%d; want kind=%d, bytes=%d", i, kind, len(got), expected.kind, len(expected.data))
		}
	}
}

func TestStreamLinkRoutingBacklogCannotStarveMetadataOrIP(t *testing.T) {
	link := &streamNodeLink{
		routing:   make(chan streamFrame, streamRoutingQueue),
		priority:  make(chan streamFrame, streamPriorityQueue),
		ipPackets: newStreamIPSendQueue(),
		stop:      make(chan struct{}),
	}
	for i := 0; i < streamRoutingQueue+10; i++ {
		if err := link.SendDatagram([]byte{PacketBabel, byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if len(link.routing) != streamRoutingQueue {
		t.Fatal("routing queue exceeded its bound")
	}
	metadata := streamFrame{kind: streamControl, data: []byte("proof")}
	link.priority <- metadata
	served := 0
	for id := uint32(1); id <= 100; id++ {
		if err := link.SendIP(id, queueTestIP(TunnelMTU, 1)); err != nil {
			t.Fatal(err)
		}
		for turn, kind := range []byte{streamPacket, streamControl, streamPacket, streamWholeIP} {
			frame, ok := link.nextFrame(&served)
			if !ok || frame.kind != kind {
				t.Fatalf("cycle %d turn %d: kind=%d, want %d", id, turn, frame.kind, kind)
			}
			switch kind {
			case streamPacket:
				if !bytes.Equal(frame.data, []byte{PacketBabel, byte((id-1)*2 + uint32(turn/2))}) {
					t.Fatal("routing packets were replaced or reordered under saturation")
				}
				// Keep the routing lane saturated while metadata/IP are waiting.
				if err := link.SendDatagram([]byte{PacketBabel, byte(streamRoutingQueue + (id-1)*2 + uint32(turn/2))}); err != nil {
					t.Fatal(err)
				}
			case streamControl:
				link.priority <- frame
			case streamWholeIP:
				if frame.sequence != id {
					t.Fatalf("routed packet %d replaced with %d", id, frame.sequence)
				}
			}
		}
	}
	close(link.stop)
	if _, ok := link.nextFrame(&served); ok {
		t.Fatal("closed link still dequeued routing traffic")
	}
}
