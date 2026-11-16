package directstream

import (
	"bytes"
	"errors"
	"testing"
)

func pool(t *testing.T, size int) *Pool {
	t.Helper()
	p, err := NewPool(size)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReceiveGrantsReserveSharedCapacity(t *testing.T) {
	p := pool(t, 1024)
	a, b := p.NewBuffer(), p.NewBuffer()
	if a.Reserve(257) != nil || b.Reserve(512) != nil || p.Available() != 0 {
		t.Fatal("unbacked grants")
	}
	if !errors.Is(a.Reserve(513), ErrCapacity) || a.Limit() != 512 {
		t.Fatal("non-atomic grant")
	}
	if err := a.Reserve(1); err != nil || a.Limit() != 512 {
		t.Fatal("policy shrink revoked credit")
	}
	if err := a.Append(bytes.Repeat([]byte{9}, 512)); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(255); err != nil || p.Available() != 0 {
		t.Fatal("partial chunk freed")
	}
	if err := a.Release(256); err != nil || p.Available() != 256 {
		t.Fatal("consumed chunk not freed")
	}
	if b.Reserve(768) != nil || p.Available() != 0 {
		t.Fatal("credit reuse failed")
	}
	a.Close()
	b.Close()
	if p.Available() != 1024 {
		t.Fatal("payload or unused credit leaked")
	}
}

func TestRecoveryReplaysOnlyReceiverMissingSuffix(t *testing.T) {
	s := Sender{Buffer: pool(t, 2048).NewBuffer()}
	r := Receiver{Buffer: pool(t, 2048).NewBuffer()}
	if r.Buffer.Reserve(2048) != nil || s.State(0, 2048) != nil {
		t.Fatal("grant")
	}
	payload := make([]byte, 1024)
	for i := range payload {
		payload[i] = byte(i / 13)
	}
	if s.Accept(payload, 2048) != nil {
		t.Fatal("accept")
	}
	var frame [512]byte
	n, err := s.Offer(frame[:], 0)
	if err != nil || n != 512 || r.Accept(frame[:n]) != nil || s.State(512, 2048) != nil {
		t.Fatal("first frame")
	}
	n, err = s.Offer(frame[:256], 512)
	// The next DATA frame owns 256 bytes; its ACK is lost with the carrier.
	if err != nil || n != 256 || r.Accept(frame[:n]) != nil {
		t.Fatal("second frame")
	}
	if s.State(r.Buffer.End(), r.Buffer.Limit()) != nil || s.Buffer.Start() != 768 {
		t.Fatal("reconcile")
	}
	n, err = s.Offer(frame[:], 768)
	if err != nil || n != 256 || r.Accept(frame[:n]) != nil {
		t.Fatal("replay")
	}
	got := make([]byte, 1024)
	if n, err = r.Buffer.CopyTo(got, 0); err != nil || n != 1024 || !bytes.Equal(got, payload) {
		t.Fatal("stream changed")
	}
	if s.State(1024, 2048) != nil || s.Buffer.pool.Available() != 2048 {
		t.Fatal("retention not freed")
	}
}

func TestAckMayRaceWriteCompletionWithoutRecyclingWriterBytes(t *testing.T) {
	p := pool(t, 512)
	s := Sender{Buffer: p.NewBuffer()}
	if s.State(0, 512) != nil || s.Accept(bytes.Repeat([]byte{7}, 512), 512) != nil {
		t.Fatal("setup")
	}
	var pending [512]byte
	if n, err := s.Offer(pending[:], 0); err != nil || n != 512 {
		t.Fatal("offer")
	}
	if s.State(512, 1024) != nil {
		t.Fatal("ACK before write completion rejected")
	}
	other := p.NewBuffer()
	if other.Reserve(512) != nil || other.Append(bytes.Repeat([]byte{8}, 512)) != nil {
		t.Fatal("reuse")
	}
	if !bytes.Equal(pending[:], bytes.Repeat([]byte{7}, 512)) {
		t.Fatal("inflight payload mutated")
	}
}

func TestFinCanOvertakeDataAndPreservesHalfClose(t *testing.T) {
	r := Receiver{Buffer: pool(t, 512).NewBuffer()}
	if r.Buffer.Reserve(512) != nil || r.Finish(300) != nil || r.EOF() {
		t.Fatal("early EOF")
	}
	if r.Accept(make([]byte, 300)) != nil || r.EOF() {
		t.Fatal("received is not consumed")
	}
	if r.Finish(301) == nil || r.Accept([]byte{0}) == nil {
		t.Fatal("changed final offset")
	}
	if r.Buffer.Release(300) != nil || !r.EOF() {
		t.Fatal("missing EOF")
	}
}
