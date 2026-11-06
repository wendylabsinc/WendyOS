//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestIPAssemblyExpiryKeepsFramedDataAndControlUsable(t *testing.T) {
	left, right := net.Pipe()
	sender, receiver := newStreamNodeLink(left), newStreamNodeLink(right)
	defer sender.Close()
	defer receiver.Close()
	ctx, cancel := context.WithTimeout(context.Background(), ipAssemblyLifetime+5*time.Second)
	defer cancel()
	var assembler IPAssembler
	_, pieces := assemblyPacket(t, TunnelMTU, 1)
	results := make(chan []byte, 1)
	errs := make(chan error, 1)
	var receiveTimeouts atomic.Int32
	go func() {
		assembler.Receive(pieces[1], time.Now())
		d, err := assembler.receiveDatagram(ctx, func(actual context.Context) ([]byte, error) {
			d, err := receiver.ReceiveDatagram(actual)
			if actual != ctx && errors.Is(err, context.DeadlineExceeded) {
				receiveTimeouts.Add(1)
			}
			return d, err
		})
		if err != nil {
			errs <- err
		} else {
			results <- d
		}
	}()
	waitAssemblyExpiry(t, &assembler, ctx)
	if receiveTimeouts.Load() != 1 {
		t.Fatal("framed receiver did not witness the owned assembly deadline")
	}
	if err := sender.WriteControl(ControlMessage{Kind: "bundle", Bundle: [][]byte{[]byte("still usable")}}); err != nil {
		t.Fatal(err)
	}
	m, err := receiver.ReadControl()
	if err != nil || m.Kind != "bundle" {
		t.Fatal("framed control lost after timeout", err)
	}
	_, whole := assemblyPacket(t, 80, 2)
	if err := sender.SendDatagram(whole[0]); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-results:
		if !bytes.Equal(got, whole[0]) {
			t.Fatal("wrong framed datagram")
		}
	case err := <-errs:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

type assemblyReadLink struct {
	packets         chan []byte
	receiveTimeouts atomic.Int32
}

func (*assemblyReadLink) Close() error              { return nil }
func (*assemblyReadLink) SendDatagram([]byte) error { return nil }
func (*assemblyReadLink) ReadControl() (ControlMessage, error) {
	return ControlMessage{}, net.ErrClosed
}
func (*assemblyReadLink) WriteControl(ControlMessage) error { return nil }
func (l *assemblyReadLink) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case d := <-l.packets:
		return d, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			l.receiveTimeouts.Add(1)
		}
		return nil, ctx.Err()
	}
}

func TestNodeReadLinkExpiresAndSnapshotsWithoutClosingCarrier(t *testing.T) {
	link := &assemblyReadLink{packets: make(chan []byte, 1)}
	p := &nodePeer{link: link}
	_, pieces := assemblyPacket(t, TunnelMTU, 1)
	p.assembler.Receive(pieces[0], time.Now())
	n := &Node{events: make(chan nodeEvent, 1)}
	n.view.Store(&NodeSnapshot{Links: []PeerLink{{assembler: &p.assembler}}})
	first := n.Snapshot()
	ctx, cancel := context.WithTimeout(context.Background(), ipAssemblyLifetime+5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- n.readLink(ctx, p) }()
	waitAssemblyExpiry(t, &p.assembler, ctx)
	if first.Links[0].Reassembly.PendingPackets != 1 || n.Snapshot().Links[0].Reassembly.PendingPackets != 0 {
		t.Fatal("node snapshot was aliased/stale")
	}
	link.packets <- []byte{PacketBabel, 7}
	select {
	case event := <-n.events:
		if !bytes.Equal(event.packet, []byte{7}) {
			t.Fatal("wrong Babel event")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if link.receiveTimeouts.Load() != 1 {
		t.Fatal("unexpected receive timeout count")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("parent cancellation swallowed", err)
	}
	if p.assembler.Snapshot().PendingBytes != 0 {
		t.Fatal("terminal read loop retained memory")
	}
}
