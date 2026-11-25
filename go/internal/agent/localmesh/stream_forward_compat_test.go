package localmesh

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

func futureTestFrame(kind byte, body []byte) []byte {
	frame := make([]byte, 5+len(body))
	frame[0] = kind
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(body)))
	copy(frame[5:], body)
	return frame
}

func TestStreamDiscardsFutureKindsAndKeepsFraming(t *testing.T) {
	left, right := net.Pipe()
	link := newStreamNodeLink(left)
	defer link.Close()
	defer right.Close()
	if err := right.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	packet := []byte{PacketBabel, 42}
	var control bytes.Buffer
	if err := WriteControl(&control, ControlMessage{Kind: "bundle", Bundle: [][]byte{[]byte("next control")}}); err != nil {
		t.Fatal(err)
	}
	wire := bytes.Join([][]byte{
		futureTestFrame(0, nil),
		futureTestFrame(254, []byte{streamControl, 0, 0, 0, 1, 255}),
		futureTestFrame(255, bytes.Repeat([]byte{streamPacket}, MaxControlMessage+4)),
		futureTestFrame(streamPacket, packet),
		futureTestFrame(streamControl, control.Bytes()),
	}, nil)
	written := make(chan error, 1)
	go func() { written <- writeFull(right, wire) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := link.ReceiveDatagram(ctx)
	if err != nil || !bytes.Equal(got, packet) {
		t.Fatalf("known packet after future kinds = %x, %v", got, err)
	}
	select {
	case m := <-link.controls:
		if m.Kind != "bundle" || len(m.Bundle) != 1 || string(m.Bundle[0]) != "next control" {
			t.Fatalf("known control after future kinds = %+v", m)
		}
	case <-link.done:
		t.Fatal("future kind closed the carrier:", link.terminalError())
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if got := link.rxBytes.Load(); got != int64(len(wire)) {
		t.Fatalf("received bytes = %d, want %d", got, len(wire))
	}
}

func TestFutureStreamKindRetainsEnvelopeBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind byte
		size uint32
		body []byte
	}{
		{"oversized-future", 255, MaxControlMessage + 5, nil},
		{"truncated-future", 255, 3, []byte{1, 2}},
		{"empty-control", streamControl, 0, nil},
		{"empty-packet", streamPacket, 0, nil},
		{"oversized-packet", streamPacket, DatagramLimit + 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, right := net.Pipe()
			link := newStreamNodeLink(left)
			defer link.Close()
			defer right.Close()
			frame := futureTestFrame(tc.kind, tc.body)
			binary.BigEndian.PutUint32(frame[1:5], tc.size)
			go func() { _, _ = right.Write(frame); _ = right.Close() }()
			select {
			case <-link.done:
				if link.terminalError() == nil {
					t.Fatal("invalid frame did not retain its error")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("invalid frame did not terminate")
			}
		})
	}
}

type futureDatagramLink struct {
	packets   [][]byte
	beforeEOF func()
}

func (*futureDatagramLink) Close() error                         { return nil }
func (*futureDatagramLink) SendDatagram([]byte) error            { return nil }
func (*futureDatagramLink) ReadControl() (ControlMessage, error) { return ControlMessage{}, io.EOF }
func (*futureDatagramLink) WriteControl(ControlMessage) error    { return nil }
func (l *futureDatagramLink) ReceiveDatagram(context.Context) ([]byte, error) {
	if len(l.packets) == 0 {
		if l.beforeEOF != nil {
			l.beforeEOF()
		}
		return nil, io.EOF
	}
	p := l.packets[0]
	l.packets = l.packets[1:]
	return p, nil
}

func TestNodeDiscardsFutureDatagramsWithoutLosingAssembly(t *testing.T) {
	packet := make([]byte, TunnelMTU)
	packet[0] = 0x60
	binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)-40))
	fragments, err := EncodeIP(7, packet)
	if err != nil {
		t.Fatal(err)
	}
	link := &futureDatagramLink{packets: [][]byte{fragments[0], {0}, bytes.Repeat([]byte{255}, DatagramLimit), {PacketBabel, 42}}}
	peer := &nodePeer{link: link}
	var beforeEOF IPAssemblyStats
	link.beforeEOF = func() { beforeEOF = peer.assembler.Snapshot() }
	node := &Node{events: make(chan nodeEvent, 1)}
	if err := node.readLink(context.Background(), peer); !errors.Is(err, io.EOF) {
		t.Fatalf("read after future datagram = %v", err)
	}
	select {
	case event := <-node.events:
		if !bytes.Equal(event.packet, []byte{42}) {
			t.Fatalf("Babel payload = %x", event.packet)
		}
	default:
		t.Fatal("known Babel datagram after future kinds was lost")
	}
	if beforeEOF.PendingPackets != 1 || beforeEOF.PendingBytes != TunnelMTU || beforeEOF.InvalidFragments != 0 {
		t.Fatalf("future kind disturbed IP assembly: %+v", beforeEOF)
	}
	if peer.assembler.Snapshot().PendingBytes != 0 {
		t.Fatal("shutdown retained the partial packet")
	}
}

func TestHelloIgnoresOptionalTransportFields(t *testing.T) {
	self, peer := testDevices(t)
	body, err := netwire.Marshal(map[string]any{
		"k": "hello", "future-envelope": []any{1, "optional"},
		"h": map[string]any{"v": StreamProtocolVersion, "d": peer.UUID(), "p": self.UUID(), "m": TunnelMTU, "g": DatagramLimit, "future-hello": map[string]any{"capability": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var framed bytes.Buffer
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	framed.Write(header[:])
	framed.Write(body)
	m, err := ReadControl(&framed)
	if err != nil || m.Kind != "hello" || !validStreamHelloVersion(m.Hello, self, peer, StreamProtocolVersion) {
		t.Fatalf("hello with optional fields = %+v, %v", m, err)
	}
}
