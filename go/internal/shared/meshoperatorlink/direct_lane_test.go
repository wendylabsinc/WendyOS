package meshoperatorlink

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
)

func TestDirectQueueBackpressureAndFairness(t *testing.T) {
	l := &RouterLink{Link: &Link{done: make(chan struct{})}, directSelected: directstream.CapabilityVersion | directstream.CapabilityTLS, directTransmit: make(chan []byte, 2), directRecords: make(chan []byte, 2), transmit: make(chan []byte, 2), routing: make(chan []byte, 2), transmitWake: make(chan struct{}, 1)}
	body := []byte{42}
	for range 2 {
		if ok, err := l.TrySendDirectRecord(body); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	body[0] = 99
	if ok, err := l.TrySendDirectRecord(body); ok || err != nil {
		t.Fatal("pressure lost retry ownership", ok, err)
	}
	l.routing <- []byte{2, 7}
	l.transmit <- []byte{1, 8}
	lastDirect, lastRouting := false, false
	for i, wantKind := range []byte{6, 4, 6, 4} {
		kind, data, ok := l.nextRouterFrame(&lastDirect, &lastRouting)
		if !ok || kind != wantKind {
			t.Fatal(i, kind, ok)
		}
		if kind == 6 && data[0] != 42 {
			t.Fatal("caller mutation reached writer")
		}
		if i == 1 && data[0] != 2 {
			t.Fatal("Babel not served")
		}
		if i == 3 && data[0] != 1 {
			t.Fatal("IP not served")
		}
	}
	if _, _, ok := l.nextRouterFrame(&lastDirect, &lastRouting); ok {
		t.Fatal("empty scheduler returned record")
	}
	for _, body := range [][]byte{nil, make([]byte, maxDirectRecord+1)} {
		if ok, err := l.TrySendDirectRecord(body); ok || err == nil {
			t.Fatal("invalid size accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.ReceiveDirectRecord(ctx); err != context.Canceled {
		t.Fatal(err)
	}
	close(l.done)
	if ok, err := l.TrySendDirectRecord([]byte{1}); ok || err == nil {
		t.Fatal("closed carrier accepted data")
	}
	if _, err := l.ReceiveDirectRecord(context.Background()); err == nil {
		t.Fatal("closed receiver blocked")
	}
}

func TestDirectFrameSizeRejectedBeforeBodyRead(t *testing.T) {
	var header [5]byte
	header[0] = 6
	for _, n := range []uint32{0, maxDirectRecord + 1, 1 << 30} {
		binary.BigEndian.PutUint32(header[1:], n)
		if _, _, err := readFrame(bytes.NewReader(header[:])); err == nil || err.Error() != "invalid operator frame size" {
			t.Fatalf("size %d: %v", n, err)
		}
	}
}
