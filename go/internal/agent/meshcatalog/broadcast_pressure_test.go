package meshcatalog

import (
	"bytes"
	"testing"
)

func TestBroadcastFullPeerQueuesDoNotCopyDroppedRecords(t *testing.T) {
	r := &Runtime{peers: make(map[int32]*catalogPeer)}
	for asset := int32(1); asset <= 100; asset++ {
		p := &catalogPeer{out: make(chan SignedRecord, 128)}
		for range cap(p.out) {
			p.out <- SignedRecord{}
		}
		r.peers[asset] = p
	}
	w := SignedRecord{Fingerprint: "fixture", Body: bytes.Repeat([]byte("x"), MaxRecordBytes), Signature: make([]byte, 64)}
	allocations := testing.AllocsPerRun(20, func() { r.Broadcast(w) })
	t.Logf("100 full peer queues: allocations per dropped broadcast=%.0f", allocations)
	if allocations != 0 {
		t.Fatalf("dropped record copied: %.0f allocations", allocations)
	}
	for _, p := range r.peers {
		if len(p.out) != cap(p.out) {
			t.Fatal("changed full queue")
		}
	}
}

func TestBroadcastAfterQueueDrainCopiesForEachPeer(t *testing.T) {
	a, b := &catalogPeer{out: make(chan SignedRecord, 1)}, &catalogPeer{out: make(chan SignedRecord, 1)}
	r := &Runtime{peers: map[int32]*catalogPeer{1: a, 2: b}}
	a.out <- SignedRecord{}
	b.out <- SignedRecord{}
	r.Broadcast(SignedRecord{Body: []byte("dropped")})
	<-a.out
	<-b.out
	w := SignedRecord{Body: []byte("withdrawal"), Signature: []byte("signature")}
	r.Broadcast(w)
	x, y := <-a.out, <-b.out
	w.Body[0], w.Signature[0] = 'X', 'X'
	x.Body[1], x.Signature[1] = 'Y', 'Y'
	if string(y.Body) != "withdrawal" || string(y.Signature) != "signature" {
		t.Fatal("queued peers alias caller or one another")
	}
}
