//go:build linux

package localmesh

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func primeFastQueue(q *streamIPSendQueue) {
	for range 30 {
		q.recordWrite(TunnelMTU, 10*time.Millisecond)
	}
}

// A fast socket write measures kernel acceptance, not RF service. In AN a
// short application DATAGRAM expired behind control work after a 180ms estimate.
func TestStreamIPQueueKeepsShortDatagramDuringControlContention(t *testing.T) {
	q := newStreamIPSendQueue()
	primeFastQueue(q)
	now := time.Now()
	if !q.enqueue(1, queueTestAppUDP(188, 1, 34975, AppSessionPort), now) {
		t.Fatal("datagram not admitted")
	}
	served := 0
	if p, ok := q.pop(now.Add(1500*time.Millisecond), &served); !ok || p.sequence != 1 {
		t.Fatalf("short datagram expired behind ordinary control contention: %+v", q.snapshot())
	}
}

func TestStreamIPQueueDoesNotShortenAdmittedGrace(t *testing.T) {
	q := newStreamIPSendQueue()
	q.recordWrite(TunnelMTU, 400*time.Millisecond)
	now := time.Now()
	q.enqueue(1, queueTestAppUDP(188, 1, 34975, AppSessionPort), now)
	primeFastQueue(q)
	served := 0
	if p, ok := q.pop(now.Add(2100*time.Millisecond), &served); !ok || p.sequence != 1 {
		t.Fatalf("later fast writes retroactively expired admitted traffic: %+v", q.snapshot())
	}
}

func TestStreamIPQueueRemembersMeasuredSlowdownForWaitingPacket(t *testing.T) {
	q := newStreamIPSendQueue()
	primeFastQueue(q)
	now := time.Now()
	q.enqueue(1, queueTestAppUDP(188, 1, 34975, AppSessionPort), now)
	q.enqueue(2, queueTestAppUDP(188, 2, 34976, AppSessionPort), now)
	q.recordWrite(TunnelMTU, 700*time.Millisecond)
	primeFastQueue(q)
	served := 0
	if p, ok := q.pop(now.Add(3*time.Second), &served); !ok || p.sequence != 1 {
		t.Fatal("recovered rate revoked the measured slow-write allowance")
	}
	if _, ok := q.pop(now.Add(5*time.Second), &served); ok {
		t.Fatal("stalled traffic escaped the five-second hard bound")
	}
}

func TestStreamLinkShortDatagramSurvivesControlWrite(t *testing.T) {
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
	primeFastQueue(link.ipPackets)
	done := make(chan error, 1)
	go func() { done <- link.WriteControl(ControlMessage{Kind: "identity-request", Principal: "test"}) }()
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("control writer did not start")
	}
	packet := queueTestAppUDP(188, 1, 34975, AppSessionPort)
	if err := link.SendIP(1, packet); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	close(gate.release)
	_ = b.SetReadDeadline(time.Now().Add(2 * time.Second))
	if kind, _ := readStreamTestFrame(t, b); kind != streamControl {
		t.Fatal("control frame lost")
	}
	kind, frame := readStreamTestFrame(t, b)
	if kind != streamPacket {
		t.Fatal("datagram did not follow control work")
	}
	var assembler IPAssembler
	got, err := assembler.Receive(frame, time.Now())
	if err != nil || !bytes.Equal(got, packet) {
		t.Fatalf("incomplete application datagram: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
