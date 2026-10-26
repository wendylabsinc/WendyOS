package meshcatalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestServiceBurstWaitsForOneOriginCertificateFetch(t *testing.T) {
	f := newFixture(t)
	origin, originCache := f.newCatalog(t, p533, "default", nil, nil)
	dest, destCache := f.newCatalog(t, p534, "default", nil, nil)
	sender, receiver := NewSynchronizer(origin, originCache), NewSynchronizer(dest, destCache)
	defer sender.Close()
	defer receiver.Close()
	var requests []Message
	for i := range 17 {
		spec := testSpec()
		spec.ServiceID = fmt.Sprintf("service-%d", i)
		wire, err := origin.Publish(spec, f.now)
		if err != nil {
			t.Fatal(err)
		}
		replies, changed, err := receiver.Receive(Message{Kind: "record", Record: &wire}, f.now)
		if err != nil || len(changed) != 0 {
			t.Fatalf("service %d before certificate completion: changes=%d err=%v", i, len(changed), err)
		}
		requests = append(requests, replies...)
	}
	if len(requests) != 1 || requests[0].Kind != "identity-request" {
		t.Fatalf("one origin's burst queued %d identity requests", len(requests))
	}
	description, _, err := sender.Receive(requests[0], f.now)
	if err != nil {
		t.Fatal(err)
	}
	changed := exchangeServiceMessages(t, sender, receiver, description, f.now)
	if len(changed) != 17 || len(dest.Snapshot(f.now)) != 17 || receiver.PendingIdentity() {
		t.Fatalf("after element fetch: admitted=%d live=%d pending=%v", len(changed), len(dest.Snapshot(f.now)), receiver.PendingIdentity())
	}
}

func TestPendingServiceRecordsBoundCountBytesAndAge(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(fmt.Sprintf("large=%v", large), func(t *testing.T) {
			f := newFixture(t)
			origin, _ := f.newCatalog(t, p533, "default", nil, nil)
			dest, cache := f.newCatalog(t, p534, "default", nil, nil)
			s := NewSynchronizer(dest, cache)
			defer s.Close()
			wire, err := origin.Publish(testSpec(), f.now)
			if err != nil {
				t.Fatal(err)
			}
			if large {
				// The envelope is structurally valid but its signature has not
				// been checked yet: hostile pending data must count too.
				wire.Body = append(wire.Body, bytes.Repeat([]byte(" "), MaxRecordBytes-len(wire.Body))...)
				wire.Signature = make([]byte, 2048)
			}
			count := 0
			for ; count <= maxPendingRecords; count++ {
				_, _, err = s.Receive(Message{Kind: "record", Record: &wire}, f.now)
				if errors.Is(err, errPendingIdentities) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if count > maxPendingRecords || count == 0 || large && count >= maxPendingRecords {
				t.Fatalf("pending limit did not bound count/bytes: %d", count)
			}
			if !large && count != maxPendingRecords {
				t.Fatalf("small envelopes hit limit unexpectedly: %d", count)
			}
			if _, _, err := s.ContinueCertificates(f.now.Add(MaxLease + time.Second)); err != nil || s.PendingIdentity() {
				t.Fatalf("expired unverified records retained: pending=%v err=%v", s.PendingIdentity(), err)
			}
		})
	}
}

func TestCertificateReplySurvivesFullInventoryQueue(t *testing.T) {
	f := newFixture(t)
	c, cache := f.newCatalog(t, p533, "default", nil, nil)
	r, _ := NewRuntime(c, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	p := &catalogPeer{syncer: NewSynchronizer(c, cache), out: make(chan []byte, maxInventoryQueueFrames), initial: inventoryMessages(nil)}
	defer p.syncer.Close()
	for range cap(p.out) {
		if !r.queue(p, inventoryMessage{Message: Message{Kind: "snapshot-done"}}) {
			t.Fatal("could not fill bulk queue")
		}
	}
	request := Message{Kind: "identity-request", Principal: p534}
	if !r.queueReplies(p, []Message{request}) || len(p.replies) != 1 {
		t.Fatal("backpressure discarded a protocol reply")
	}
	first := <-p.out
	p.queued.Add(-int64(len(first)))
	r.pump(p)
	if len(p.replies) != 0 || p.replyBytes != 0 || len(p.initial) == 0 {
		t.Fatal("reply was not queued before more bulk inventory")
	}
	for range cap(p.out) - 1 {
		<-p.out
	}
	var actual inventoryMessage
	if err := json.Unmarshal(<-p.out, &actual); err != nil || actual.Kind != request.Kind || actual.Principal != request.Principal {
		t.Fatalf("requested reply missing behind admitted bulk frames: %+v %v", actual, err)
	}
	// Sustained overload remains bounded and preserves already admitted data.
	for range cap(p.out) {
		p.out <- []byte("occupied")
	}
	for range maxInventoryQueueFrames {
		if !r.queueReplies(p, []Message{request}) {
			t.Fatal("bounded reply reserve filled too early")
		}
	}
	before := p.replyBytes
	if r.queueReplies(p, []Message{request}) || len(p.replies) != maxInventoryQueueFrames || p.replyBytes != before {
		t.Fatal("overload replaced or exceeded admitted reply reserve")
	}
}

func TestCertificateExchangeCanInterleaveInitialInventory(t *testing.T) {
	f := newFixture(t)
	origin, _ := f.newCatalog(t, p533, "default", nil, nil)
	dest, cache := f.newCatalog(t, p534, "default", nil, nil)
	r, _ := NewRuntime(dest, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	p := &catalogPeer{id: 1, syncer: NewSynchronizer(dest, cache), out: make(chan []byte, maxInventoryQueueFrames)}
	defer p.syncer.Close()
	r.peers[p.id] = p
	description := localmesh.DescribeCertificates(origin.principal, origin.creds.Certificate.Certificate)
	wire, err := inventoryWire(inventoryMessage{Message: Message{Kind: "certificate", Certificate: description}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.receive(p, wire); err != nil {
		t.Fatal("certificate repair rejected during initial inventory", err)
	}
	if p.inventory.done || len(p.out) != 1 {
		t.Fatalf("certificate fetch changed bootstrap state: done=%v queued=%d", p.inventory.done, len(p.out))
	}
	var request inventoryMessage
	if err := json.Unmarshal(<-p.out, &request); err != nil || request.Certificate == nil || request.Certificate.Request == nil {
		t.Fatal("missing element request during inventory", err)
	}
}

func TestRetiredFreshnessNonceCannotFetchOrRestoreProjection(t *testing.T) {
	f := newFixture(t)
	origin, _ := f.newCatalog(t, p533, "default", nil, nil)
	dest, cache := f.newCatalog(t, p534, "default", nil, nil)
	r, _ := NewRuntime(dest, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	p := &catalogPeer{syncer: NewSynchronizer(dest, cache), out: make(chan []byte, maxInventoryQueueFrames)}
	defer p.syncer.Close()
	q := FreshRequest{Origin: p533, Requester: p534, Nonce: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Hops: 32}
	proof, err := origin.signInventory(q, f.now)
	if err != nil {
		t.Fatal(err)
	}
	state := &freshRequestState{query: q, expires: f.now.Add(freshLifetime), data: proof, next: 1, total: 1}
	r.pendingFresh[origin.device] = q.Nonce
	r.requests[q.Nonce] = state
	r.InvalidateProjection(origin.device)
	// A healed route may have a new challenge already pending. The old
	// response must neither fetch certificates nor affect that challenge.
	r.pendingFresh[origin.device] = "new challenge after route heal"
	if err := r.finishFresh(p, q.Nonce, state, f.now); err != nil {
		t.Fatal(err)
	}
	if r.fresh[origin.device] || len(p.out) != 0 || len(r.requests) != 0 || p.syncer.certificates.Pending() || len(dest.history) != 0 {
		t.Fatal("pre-partition response changed current verification state")
	}
	if r.pendingFresh[origin.device] != "new challenge after route heal" {
		t.Fatal("old proof consumed the replacement challenge")
	}
}
