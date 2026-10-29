//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/WendyOS/go/babel"
)

// Keep the production QUIC adapter's method set and capability in these Node
// fixtures. Only socket I/O is replaced; the real adapter still chooses and
// accounts for the carrier framing used by readTUN.
type nodeCarrierFixture struct {
	*quicNodeLink
	incoming chan []byte
	closed   int
}

func (l *nodeCarrierFixture) Close() error { l.closed++; return nil }
func (l *nodeCarrierFixture) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case packet := <-l.incoming:
		return packet, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestCarrierNodeTUNUsesNegotiatedAdmissionWithoutClosingOnLoss(t *testing.T) {
	terminal := errors.New("closed carrier")
	for _, tc := range []struct {
		name            string
		capacity        int64
		negotiated      int
		outcomes        []error
		calls, accepted int
		closed          int
		wantErr         error
	}{
		{name: "whole", capacity: 1356, calls: 1, accepted: 1, wantErr: io.EOF},
		{name: "fixed", capacity: 1156, calls: 2, accepted: 2, wantErr: io.EOF},
		{name: "peer constrained", capacity: 1356, negotiated: DatagramLimit, calls: 2, accepted: 2, wantErr: io.EOF},
		{name: "queue loss", capacity: 1356, outcomes: []error{quic.ErrDatagramQueueFull}, calls: 1, wantErr: io.EOF},
		{name: "size loss", capacity: 1356, outcomes: []error{&quic.DatagramTooLargeError{MaxDatagramPayloadSize: 1156}, &quic.DatagramTooLargeError{MaxDatagramPayloadSize: 1000}}, calls: 2, wantErr: io.EOF},
		{name: "terminal", capacity: 1356, outcomes: []error{terminal}, calls: 1, closed: 1, wantErr: terminal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admission := &admissionFixture{capacity: tc.capacity, outcomes: tc.outcomes}
			limit := tc.negotiated
			if limit == 0 {
				limit = WholeIPDatagramLimit
			}
			link := &nodeCarrierFixture{quicNodeLink: &quicNodeLink{sendIP: &carrierIPSender{sender: admission, negotiated: limit}}}
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			defer write.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			destination := netip.MustParseAddr("fd45:b891:567f::42")
			n := &Node{forwardRoutes: []babel.Route{{Prefix: netip.PrefixFrom(destination, 128), Link: 7}}}
			packet, _ := assemblyPacket(t, TunnelMTU, 1)
			copy(packet[24:40], destination.AsSlice())
			peer := &nodePeer{id: 7, link: link, tun: read}
			done := make(chan error, 1)
			joined := make(chan struct{})
			go func() { defer close(joined); done <- n.readTUN(ctx, peer) }()
			defer func() {
				read.Close()
				<-joined
			}()
			if _, err := write.Write(packet); err != nil {
				t.Fatal(err)
			}
			write.Close()
			select {
			case err := <-done:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Node result: %v, want %v", err, tc.wantErr)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if len(admission.calls) != tc.calls || len(admission.accepted) != tc.accepted || link.closed != tc.closed {
				t.Fatalf("Node bypassed carrier admission or closed on loss: calls=%d accepted=%d closed=%d", len(admission.calls), len(admission.accepted), link.closed)
			}
			assembler := IPAssembler{wholeIP: true}
			var got []byte
			for _, d := range admission.accepted {
				got, err = assembler.Receive(d, time.Now())
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.accepted > 0 && !bytes.Equal(got, packet) {
				t.Fatal("Node framing changed routed IP bytes")
			}
		})
	}
}

func TestCarrierNodeReceiveCapabilityDoesNotWidenOtherFrames(t *testing.T) {
	packet, _ := assemblyPacket(t, TunnelMTU, 1)
	whole := wholeIPFrame(1, packet)
	for _, tc := range []struct {
		name       string
		capability int
		data       []byte
		accept     bool
	}{
		{"whole router", WholeIPDatagramLimit, whole, true},
		{"baseline router", DatagramLimit, whole, false},
		{"no capability", 0, whole, false},
		{"Babel stays bounded", WholeIPDatagramLimit, append([]byte{PacketBabel}, make([]byte, DatagramLimit)...), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link := &nodeCarrierFixture{quicNodeLink: &quicNodeLink{receiveIP: tc.capability}, incoming: make(chan []byte, 2)}
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			defer write.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			n := &Node{events: make(chan nodeEvent, 1)}
			peer := &nodePeer{link: link, tun: write}
			done := make(chan error, 1)
			go func() { done <- n.readLink(ctx, peer) }()
			link.incoming <- tc.data
			if tc.accept {
				// The later Babel event witnesses that IP delivery finished and
				// the same carrier remains usable, without polling owner state.
				link.incoming <- []byte{PacketBabel, 7}
				select {
				case event := <-n.events:
					if !bytes.Equal(event.packet, []byte{7}) {
						t.Error("wrong Babel event")
					}
				case <-ctx.Done():
					t.Error(ctx.Err())
				}
				cancel()
			}
			var ended error
			select {
			case ended = <-done:
			case <-ctx.Done():
				cancel()
				ended = <-done
			}
			if tc.accept {
				if !errors.Is(ended, context.Canceled) {
					t.Fatalf("whole router did not survive: %v", ended)
				}
				write.Close()
				got, err := io.ReadAll(read)
				if err != nil || !bytes.Equal(got, packet) || peer.assembler.Snapshot().WholePackets != 1 {
					t.Fatal("Node did not opt into exact whole-IP delivery", err)
				}
			} else if ended == nil || errors.Is(ended, context.DeadlineExceeded) || peer.assembler.Snapshot().WholePackets != 0 {
				t.Fatalf("Node widened the wrong receive contract: %v", ended)
			}
		})
	}
}

func TestCarrierNodeBabelRefusalKeepsHealthyLink(t *testing.T) {
	terminal := errors.New("closed carrier")
	for _, tc := range []struct {
		name                  string
		err                   error
		closed                int
		queueDrops, sizeDrops uint64
	}{
		{"queue", quic.ErrDatagramQueueFull, 0, 1, 0},
		{"size", &quic.DatagramTooLargeError{MaxDatagramPayloadSize: 0}, 0, 0, 1},
		{"terminal", terminal, 1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admission := &admissionFixture{capacity: 1156, outcomes: []error{tc.err}}
			sender := &carrierIPSender{sender: admission, negotiated: WholeIPDatagramLimit}
			link := &nodeCarrierFixture{quicNodeLink: &quicNodeLink{sendIP: sender}}
			n := &Node{peers: map[babel.LinkID]*nodePeer{7: {link: link}}}
			if err := n.Send(babel.Datagram{Link: 7, Payload: []byte{7}}); err != nil {
				t.Fatal(err)
			}
			stats := sender.snapshot()
			if link.closed != tc.closed || stats.QueueDrops != tc.queueDrops || stats.SizeDrops != tc.sizeDrops || stats.Fragments != 0 || stats.Bytes != 0 || len(admission.calls) != 1 {
				t.Fatalf("Babel refusal closed a healthy link or changed IP counters: closed=%d stats=%+v", link.closed, stats)
			}
		})
	}
}
