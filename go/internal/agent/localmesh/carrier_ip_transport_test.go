package localmesh

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

func TestCarrierWorkspaceQUICOwnedCopies(t *testing.T) {
	a, b := testCredentials(t)
	serverTLS, err := b.PeerTLS(a.Device)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := quic.ListenAddr("127.0.0.1:0", serverTLS, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientTLS, err := a.PeerTLS(b.Device)
	if err != nil {
		t.Fatal(err)
	}
	client, err := quic.DialAddr(ctx, listener.Addr().String(), clientTLS, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseWithError(0, "test done")
	server, err := listener.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer server.CloseWithError(0, "test done")

	// Force fixed-form framing regardless of loopback PMTU. Every second
	// fragment and subsequent packet overwrites the same sender workspace.
	sender := &carrierIPSender{sender: client, negotiated: DatagramLimit}
	want := make(map[uint32][]byte)
	for id := uint32(1); id <= 8; id++ {
		packet, _ := assemblyPacket(t, TunnelMTU, id)
		packet[len(packet)-1] = byte(id)
		want[id] = bytes.Clone(packet)
		if err := sender.sendIP(id, packet); err != nil {
			t.Fatal(err)
		}
		clear(packet)
	}
	stats := sender.snapshot()
	if stats.Fragments != 16 || stats.QueueDrops != 0 {
		t.Fatalf("admission: %+v", stats)
	}
	frames := make([][]byte, 0, 16)
	for range 16 {
		frame, err := server.ReceiveDatagram(ctx)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	verifyCarrierPackets(t, frames, want)

	// Stop draining unreliable data and fill admission. Reliable control traffic
	// must still make progress on this connection after queue refusals.
	packet, _ := assemblyPacket(t, TunnelMTU, 9)
	for id := uint32(9); id < 10009 && sender.snapshot().QueueDrops == 0; id++ {
		if err := sender.sendIP(id, packet); err != nil {
			t.Fatal(err)
		}
	}
	if sender.snapshot().QueueDrops == 0 {
		t.Fatal("did not exercise full admission")
	}
	control, err := client.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Write([]byte("control")); err != nil {
		t.Fatal(err)
	}
	received, err := server.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := received.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 7)
	if _, err := io.ReadFull(received, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "control" {
		t.Fatal("control changed under datagram pressure")
	}
}
