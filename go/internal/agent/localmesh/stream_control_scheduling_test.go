package localmesh

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type schedulingGateConn struct {
	net.Conn
	entered, release chan struct{}
	once             sync.Once
}

func (c *schedulingGateConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.Conn.Write(p)
}
func readSchedulingFrame(t *testing.T, c net.Conn) (byte, []byte) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	var h [5]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, binary.BigEndian.Uint32(h[1:]))
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	return h[0], b
}
func TestStreamBabelPassesBetweenWholeIPFragments(t *testing.T) {
	a, b := net.Pipe()
	gate := &schedulingGateConn{Conn: a, entered: make(chan struct{}), release: make(chan struct{})}
	l := newStreamNodeLink(gate)
	t.Cleanup(func() { _ = l.Close(); _ = b.Close() })
	defer func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	}()
	packet := make([]byte, 1200)
	packet[0] = 0x60
	binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)-40))
	if err := l.SendIP(42, packet); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("first fragment not active")
	}
	hello := []byte{PacketBabel, 7}
	if err := l.SendDatagram(hello); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	var assembler IPAssembler
	for i := 0; i < 3; i++ {
		kind, data := readSchedulingFrame(t, b)
		if kind != streamPacket {
			t.Fatal(kind)
		}
		if i == 1 {
			if !bytes.Equal(data, hello) {
				t.Fatalf("Babel waits for whole IP: %x", data)
			}
			continue
		}
		got, err := assembler.Receive(data, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 && !bytes.Equal(got, packet) {
			t.Fatal("interleaved IP failed reassembly")
		}
	}
}
func TestStreamReceiveIPOverloadDoesNotBlockBabelOrControls(t *testing.T) {
	a, b := net.Pipe()
	l := newStreamNodeLink(a)
	t.Cleanup(func() { _ = l.Close(); _ = b.Close() })
	var control bytes.Buffer
	if err := WriteControl(&control, ControlMessage{Kind: "bundle", Bundle: [][]byte{[]byte("proof")}}); err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() {
		_ = b.SetWriteDeadline(time.Now().Add(time.Second))
		for range 1000 {
			if err := writeFull(b, futureTestFrame(streamPacket, []byte{PacketIP, 7})); err != nil {
				written <- err
				return
			}
		}
		if err := writeFull(b, futureTestFrame(streamPacket, []byte{PacketBabel, 42})); err != nil {
			written <- err
			return
		}
		written <- writeFull(b, futureTestFrame(streamControl, control.Bytes()))
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("receive IP overload blocks parser")
	}
	select {
	case m := <-l.controls:
		if m.Kind != "bundle" {
			t.Fatal(m.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("control did not pass IP overload")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := l.ReceiveDatagram(ctx)
	if err != nil || !bytes.Equal(got, []byte{PacketBabel, 42}) {
		t.Fatalf("Babel behind congested IP: %x %v", got, err)
	}
}

func TestStreamReceiveRoutingReserveStillServicesIP(t *testing.T) {
	l := &streamNodeLink{routingPackets: make(chan []byte, 32), packets: make(chan []byte, 32), done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for range 1000 {
		l.routingPackets <- []byte{PacketBabel, 1}
		l.routingPackets <- []byte{PacketBabel, 2}
		l.packets <- []byte{PacketIP, 7}
		gotIP := false
		for turn := range 3 {
			packet, err := l.ReceiveDatagram(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if packet[0] == PacketIP {
				if turn > 1 {
					t.Fatal("IP waited behind multiple Babel frames")
				}
				gotIP = true
			}
		}
		if !gotIP {
			t.Fatal("routing receive overload starved IP")
		}
	}
}
