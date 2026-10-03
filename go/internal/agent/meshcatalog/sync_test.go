package meshcatalog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestWarmReconnectSkipsBundleAndColdReceiverRepairs(t *testing.T) {
	f := newFixture(t)
	senderCatalog, senderCache := f.newCatalog(t, 533, "default", nil, nil)
	w, err := senderCatalog.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	first := NewSynchronizer(senderCatalog, senderCache).Record(w, f.now)
	if len(first) != 2 || first[0].Kind != "bundle" {
		t.Fatalf("cold session = %+v", first)
	}
	warmSender := NewSynchronizer(senderCatalog, senderCache)
	warmSender.SeedKnownBundles([]string{w.Fingerprint})
	warm := warmSender.Record(w, f.now.Add(time.Second))
	if len(warm) != 1 || warm[0].Kind != "record" {
		t.Fatalf("warm reconnect = %+v", warm)
	}
	var coldWire, warmWire bytes.Buffer
	for _, m := range first {
		if err := WriteMessage(&coldWire, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteMessage(&warmWire, warm[0]); err != nil {
		t.Fatal(err)
	}
	t.Logf("org64 fixture catalog reconnect bytes: cold=%d warm=%d", coldWire.Len(), warmWire.Len())
	if warmWire.Len() >= coldWire.Len() {
		t.Fatal("warm reconnect did not save certificate bundle bytes")
	}
	// A receiver restarted with no cached public chain asks for it. The
	// response is independently validated before the pending record can land.
	freshCatalog, freshCache := f.newCatalog(t, 534, "default", nil, nil)
	receiver := NewSynchronizer(freshCatalog, freshCache)
	request, changed, err := receiver.Receive(warm[0], f.now.Add(time.Second))
	if err != nil || len(changed) != 0 || len(request) != 1 || request[0].Kind != "identity-request" {
		t.Fatalf("cold receiver request = %+v %+v %v", request, changed, err)
	}
	bundle, _, err := warmSender.Receive(request[0], f.now.Add(time.Second))
	if err != nil || len(bundle) != 1 || bundle[0].Kind != "bundle" {
		t.Fatalf("identity repair reply = %+v %v", bundle, err)
	}
	_, changed, err = receiver.Receive(bundle[0], f.now.Add(time.Second))
	if err != nil || len(changed) != 1 || len(freshCatalog.Snapshot(f.now.Add(time.Second))) != 1 {
		t.Fatalf("repaired record = %+v %v", changed, err)
	}
	tampered := cloneWire(w)
	tampered.Signature[0] ^= 0xff
	if _, _, err := receiver.Receive(Message{Kind: "record", Record: &tampered}, f.now.Add(2*time.Second)); err == nil {
		t.Fatal("warm identity hint bypassed origin signature verification")
	}
}

func TestWarmBundleHintHasColdReceiverPendingBound(t *testing.T) {
	f := newFixture(t)
	c, cache := f.newCatalog(t, 533, "default", nil, nil)
	s := NewSynchronizer(c, cache)
	var records []SignedRecord
	for i := 0; i < 6; i++ {
		spec := testSpec()
		spec.ServiceID = fmt.Sprintf("http-%d", i)
		w, err := c.Publish(spec, f.now)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, w)
	}
	s.SeedKnownBundles([]string{records[0].Fingerprint})
	for i, w := range records {
		messages := s.Record(w, f.now)
		if i < 4 && (len(messages) != 1 || messages[0].Kind != "record") {
			t.Fatalf("record %d should use warm hint: %+v", i, messages)
		}
		if i == 4 && (len(messages) != 2 || messages[0].Kind != "bundle") {
			t.Fatalf("record %d should proactively repair cold receiver: %+v", i, messages)
		}
	}
}

func TestSynchronizationRepairsReorderAndReconnect(t *testing.T) {
	f := newFixture(t)
	a, aCache := f.newCatalog(t, 533, "default", nil, nil)
	b, bCache := f.newCatalog(t, 534, "default", nil, nil)
	c, cCache := f.newCatalog(t, 535, "default", nil, nil)
	w, err := c.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	sender := NewSynchronizer(c, cCache)
	frames := sender.Record(w, f.now)
	if len(frames) != 2 || frames[0].Kind != "bundle" || frames[1].Kind != "record" {
		t.Fatalf("first send = %+v", frames)
	}
	if again := sender.Record(w, f.now); len(again) != 0 {
		t.Fatalf("unchanged record resent = %+v", again)
	}
	toB := NewSynchronizer(b, bCache)
	replies, changed, err := toB.Receive(frames[1], f.now)
	if err != nil || len(changed) != 0 || len(replies) != 1 || replies[0].Kind != "identity-request" {
		t.Fatalf("record before bundle = %+v %+v %v", replies, changed, err)
	}
	replies, changed, err = toB.Receive(frames[0], f.now)
	if err != nil || len(replies) != 0 || len(changed) != 1 {
		t.Fatalf("bundle repair = %+v %+v %v", replies, changed, err)
	}
	if got := b.Snapshot(f.now); len(got) != 1 || got[0].Key.Asset != 535 {
		t.Fatalf("relay snapshot = %+v", got)
	}
	toA := NewSynchronizer(a, aCache)
	for _, frame := range toB.Record(changed[0], f.now) {
		_, _, err = toA.Receive(frame, f.now)
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := a.Snapshot(f.now); len(got) != 1 || got[0].Key.Asset != 535 {
		t.Fatalf("two-hop snapshot = %+v", got)
	}
	// A new link's anti-entropy sends the current record and certificate once.
	reconnected := NewSynchronizer(b, bCache)
	if messages := reconnected.Reconcile(f.now.Add(time.Second)); len(messages) != 2 {
		t.Fatalf("reconnect messages = %v", messages)
	}
	withdraw, err := c.Remove("com.wendy.test", "http", f.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range sender.Record(withdraw, f.now.Add(time.Second)) {
		_, changed, err = toB.Receive(frame, f.now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(changed) != 1 || len(b.Snapshot(f.now.Add(time.Second))) != 0 {
		t.Fatal("withdrawal did not remove service")
	}
	for _, frame := range toB.Record(changed[0], f.now.Add(time.Second)) {
		_, _, err = toA.Receive(frame, f.now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(a.Snapshot(f.now.Add(time.Second))) != 0 {
		t.Fatal("relayed withdrawal did not remove service")
	}
	if _, _, err := toA.Receive(frames[1], f.now.Add(2*time.Second)); err != nil && !errors.Is(err, ErrStale) {
		t.Fatalf("stale replay should be ignored: %v", err)
	}
}

func TestSynchronizerReconcileSendsOnlyUnsentGenerations(t *testing.T) {
	f := newFixture(t)
	c, cache := f.newCatalog(t, 533, "default", nil, nil)
	remote, remoteCache := f.newCatalog(t, 534, "default", nil, nil)
	s := NewSynchronizer(c, cache)
	receiver := NewSynchronizer(remote, remoteCache)
	first, err := c.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Reconcile(f.now); len(got) != 2 || got[0].Kind != "bundle" || got[1].Kind != "record" {
		t.Fatalf("initial snapshot = %+v", got)
	} else {
		for _, m := range got {
			if _, _, err := receiver.Receive(m, f.now); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < 3; i++ {
		if got := s.Reconcile(f.now.Add(time.Duration(i+1) * time.Second)); len(got) != 0 {
			t.Fatalf("idle pass %d sent %d messages", i, len(got))
		}
	}
	if got := s.Record(first, f.now.Add(4*time.Second)); len(got) != 0 {
		t.Fatalf("duplicate broadcast sent %+v", got)
	}
	// A Broadcast dropped before Record reaches this peer is repaired by the
	// next reconciliation, and a refreshed generation is sent exactly once.
	second, err := c.Publish(testSpec(), f.now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Reconcile(f.now.Add(5 * time.Second)); len(got) != 1 || got[0].Kind != "record" ||
		!bytes.Equal(got[0].Record.Body, second.Body) {
		t.Fatalf("lost Broadcast repair = %+v", got)
	} else if _, changed, err := receiver.Receive(got[0], f.now.Add(5*time.Second)); err != nil || len(changed) != 1 {
		t.Fatalf("refreshed signed record = %d changes, %v", len(changed), err)
	}
	if got := s.Reconcile(f.now.Add(6 * time.Second)); len(got) != 0 {
		t.Fatalf("refreshed generation resent = %+v", got)
	}
	if got := s.Record(first, f.now.Add(6*time.Second)); len(got) != 0 {
		t.Fatalf("late old-generation Broadcast sent %+v", got)
	}
	// A replacement session has no per-peer send state and sends its complete
	// current snapshot, including the origin identity needed to verify it.
	if got := NewSynchronizer(c, cache).Reconcile(f.now.Add(6 * time.Second)); len(got) != 2 {
		t.Fatalf("reconnect snapshot = %+v", got)
	}
	if got := s.Reconcile(f.now.Add(MaxLease + 6*time.Second)); len(got) != 0 || len(s.sentRecords) != 0 {
		t.Fatalf("expired record retained: messages=%d sent=%d", len(got), len(s.sentRecords))
	}
	if got := remote.Snapshot(f.now.Add(96 * time.Second)); len(got) != 0 {
		t.Fatalf("idle dedupe extended signed expiry: %+v", got)
	}
}

func TestMessageFramingRejectsMalformedAndTruncatedInput(t *testing.T) {
	var buf bytes.Buffer
	m := Message{Kind: "identity-request", Fingerprint: string(bytes.Repeat([]byte{'a'}, 64))}
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMessage(&buf)
	if err != nil || got.Kind != m.Kind || got.Fingerprint != m.Fingerprint {
		t.Fatalf("framing roundtrip = %+v %v", got, err)
	}
	var huge [4]byte
	binary.BigEndian.PutUint32(huge[:], MaxMessageBytes+1)
	if _, err := ReadMessage(bytes.NewReader(huge[:])); err == nil {
		t.Fatal("oversized frame accepted")
	}
	if _, err := ReadMessage(bytes.NewReader([]byte{0, 0, 0, 5, '{'})); err == nil {
		t.Fatal("truncated frame accepted")
	}
	if err := WriteMessage(&buf, Message{Kind: "bundle", Bundle: [][]byte{{1}}, Fingerprint: m.Fingerprint}); err == nil {
		t.Fatal("ambiguous bundle accepted")
	}
}

func TestAlreadySentRecordSkipsUnusedIdentityButNewRecordRevalidates(t *testing.T) {
	f := newFixture(t)
	c, _ := f.newCatalog(t, 533, "default", nil, nil)
	calls := 0
	trusted := true
	counted, err := localmesh.OpenIdentityCache("", localmesh.DefaultCacheLimits(), func(chain [][]byte, at time.Time) (localmesh.Identity, error) {
		calls++
		if !trusted {
			return localmesh.Identity{}, errors.New("revoked")
		}
		return f.creds[533].Verify(chain, at)
	}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = counted.Put(f.creds[533].Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	w, err := c.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSynchronizer(c, counted)
	if len(s.Record(w, f.now)) != 2 {
		t.Fatal("initial bundle/record missing")
	}
	calls = 0
	trusted = false
	if out := s.Record(w, f.now.Add(time.Second)); len(out) != 0 || calls != 0 {
		t.Fatalf("identical sent record: output=%d unused verifications=%d", len(out), calls)
	}
	next, err := c.Publish(testSpec(), f.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if out := s.Record(next, f.now.Add(time.Second)); len(out) != 0 || calls != 1 {
		t.Fatalf("new record bypassed current trust: output=%d calls=%d", len(out), calls)
	}
	// Reconciliation/projection independently call the catalog's actual cache.
	// Expiring it cannot resurrect records merely because their stamps exist.
	c.cache = counted
	if len(c.Records(f.now.Add(time.Second))) != 0 || len(c.Snapshot(f.now.Add(time.Second))) != 0 {
		t.Fatal("revoked records remained eligible")
	}
}
