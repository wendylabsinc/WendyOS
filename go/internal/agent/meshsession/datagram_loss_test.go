package meshsession

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
)

// Deterministic fragment loss/reordering happens before real authenticated
// QUIC DATAGRAM admission. Missing app data must not be repaired by a stream,
// delivered as a prefix, or hold a later complete datagram behind it.
func TestUDPRealQUICLossReorderAndDuplicateBoundaries(t *testing.T) {
	a, b := fixtureCredentials(t)
	registry := meshingress.NewRegistry()
	port := udpLifecycleApp(t, registry, "loss-echo")
	address := lifecycleServer(t, b, testUDPAuthorization{registry}, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg, err := a.PeerTLSWithTickets(b.Device, ALPN, "app-quic")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := quic.DialAddr(ctx, address.String(), cfg, quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "done")
	control, err := requestAppPort(ctx, conn, port, udpOpenMagic)
	if err != nil {
		t.Fatal(err)
	}
	control.CancelRead(0)
	control.Close()
	manager := udpmessage.NewManager()
	defer manager.Close()
	endpoint, err := manager.Open(b.Device, func(send func() error) error { return send() })
	if err != nil {
		t.Fatal(err)
	}
	admission := quicUDPAdmission(conn)
	var held []byte
	failures := make(chan error, 4)
	if err := endpoint.SetTransport(1000, false, func(frame []byte) error {
		id := binary.BigEndian.Uint64(frame[4:12])
		index := binary.BigEndian.Uint16(frame[16:18])
		if id == 1 && index == 3 {
			return nil
		} // admitted but lost, with no retransmission
		if id == 3 && index == 0 {
			held = bytes.Clone(frame)
			return nil
		}
		if err := admission(frame); err != nil {
			return err
		}
		if id == 3 && index == 1 {
			// Less than one QUIC queue of data is outstanding in this fixture.
			// A partial multi-admission failure is a fixture failure, never a retry.
			for _, duplicate := range [][]byte{frame, held} {
				if err := admission(duplicate); err != nil {
					failures <- err
				}
			}
			held = nil
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	flow := &DatagramFlow{conn: conn, endpoint: endpoint}
	defer flow.Close()
	missing := bytes.Repeat([]byte{0xa1}, 8192)
	later := []byte("later complete message")
	reordered := bytes.Repeat([]byte{0xb2}, 8192)
	for _, payload := range [][]byte{missing, later, reordered} {
		if err := flow.Send(payload); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for range 2 {
		got, err := readUDPMessage(flow, ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, later) && !bytes.Equal(got, reordered) {
			t.Fatalf("partial/lost message delivered: %d bytes", len(got))
		}
		if seen[string(got)] {
			t.Fatal("duplicate application delivery")
		}
		seen[string(got)] = true
	}
	if !seen[string(later)] || !seen[string(reordered)] {
		t.Fatal("later complete message stalled")
	}
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	if data, err := readUDPMessage(flow, short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost/duplicate message delivered after completion: %d %v", len(data), err)
	}
	select {
	case err := <-failures:
		t.Fatal("fixture transport admission failed", err)
	default:
	}
}
