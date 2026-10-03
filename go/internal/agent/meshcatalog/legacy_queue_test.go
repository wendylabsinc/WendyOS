package meshcatalog

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

func TestLegacyQueueAtomicBoundsAndRelease(t *testing.T) {
	small := Message{Kind: "identity-request", Fingerprint: fmt.Sprintf("%064d", 1)}
	var wire bytes.Buffer
	if err := WriteMessage(&wire, small); err != nil {
		t.Fatal(err)
	}
	var q legacyQueue
	for i := 0; i < maxLegacyQueueFrames; i++ {
		if err := q.add(small); err != nil {
			t.Fatal(err)
		}
	}
	if q.bytes != maxLegacyQueueFrames*wire.Len() {
		t.Fatalf("encoded accounting: %d", q.bytes)
	}
	if q.add(small) == nil || len(q.messages) != maxLegacyQueueFrames {
		t.Fatal("frame overload admitted")
	}
	old := q.messages
	for len(q.messages) > 0 {
		q.pop()
	}
	if q.bytes != 0 || q.messages != nil {
		t.Fatal("empty queue retains storage")
	}
	for _, entry := range old {
		if entry.message.Kind != "" || entry.bytes != 0 {
			t.Fatal("popped slot retains message")
		}
	}
	big := Message{Kind: "bundle", Bundle: [][]byte{make([]byte, 35<<10)}}
	if err := q.add(big, big); err != nil {
		t.Fatal(err)
	}
	before := q.bytes
	if q.add(small, big) == nil || q.bytes != before || len(q.messages) != 2 {
		t.Fatal("byte overload partly admitted")
	}
	if q.add(small, Message{Kind: "invalid"}) == nil || q.bytes != before || len(q.messages) != 2 {
		t.Fatal("invalid batch partly admitted")
	}
	if q.add(Message{Kind: "bundle", Bundle: [][]byte{make([]byte, MaxMessageBytes)}}) == nil {
		t.Fatal("oversize frame admitted")
	}
}

func TestLegacyReconcileOverflowFreshSynchronizerRepairs(t *testing.T) {
	f := newFixture(t)
	origin, _ := f.newCatalog(t, 533, "default", nil, nil)
	remote, _ := f.newCatalog(t, 534, "default", nil, nil)
	syncer := NewSynchronizer(origin, origin.cache)
	for i := 0; i < 24; i++ {
		spec := testSpec()
		spec.ServiceID = fmt.Sprintf("service-%d", i)
		if _, err := origin.Publish(spec, f.now); err != nil {
			t.Fatal(err)
		}
	}
	messages := syncer.Reconcile(f.now)
	var q legacyQueue
	if q.add(messages...) == nil || len(q.messages) != 0 || q.bytes != 0 {
		t.Fatal("oversized reconcile partially admitted")
	}
	if len(syncer.Reconcile(f.now)) != 0 {
		t.Fatal("fixture did not mark attempted stamps")
	}
	var encoded bytes.Buffer
	for _, m := range messages {
		if err := WriteMessage(&encoded, m); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("catalog snapshot: 24 records, %d frames, %d encoded bytes; queue bound %d frames/%d bytes (temporary reconcile batch separate)", len(messages), encoded.Len(), maxLegacyQueueFrames, maxLegacyQueueBytes)
	// A reconnect must create a fresh synchronizer: it repairs stamps from a
	// rejected batch and transmits a newer withdrawal instead of stale data.
	if _, err := origin.Remove(testSpec().AppID, "service-0", f.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	receiver := NewSynchronizer(remote, remote.cache)
	fresh := NewSynchronizer(origin, origin.cache)
	for _, m := range fresh.Reconcile(f.now.Add(time.Second)) {
		if _, _, err := receiver.Receive(m, f.now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(remote.Snapshot(f.now.Add(time.Second))); got != 23 {
		t.Fatalf("reconnect live records=%d", got)
	}
	if got := len(remote.Records(f.now.Add(time.Second))); got != 24 {
		t.Fatalf("withdrawal tombstone absent: %d", got)
	}
}
