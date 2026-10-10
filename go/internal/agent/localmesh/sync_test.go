package localmesh

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
	"time"
)

func TestControlBoundAndFraming(t *testing.T) {
	var b bytes.Buffer
	m := ControlMessage{Kind: "identity-request", Fingerprint: string(bytes.Repeat([]byte{'a'}, 64))}
	if err := WriteControl(&b, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadControl(&b)
	if err != nil || got.Kind != m.Kind || got.Fingerprint != m.Fingerprint {
		t.Fatal(got, err)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], MaxControlMessage+1)
	if _, err = ReadControl(bytes.NewReader(header[:])); err == nil {
		t.Fatal("oversize allocated")
	}
}

func TestDirectorySyncBundleOnceAndMissingBundleRecovery(t *testing.T) {
	source, chain, key, now := directoryFixture(t)
	m := Manifest{Version: 1, Org: 64, Asset: 445, Revision: 1, Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), AgentPort: 50052}
	w, err := SignManifest(m, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), source.cache.verify, now)
	if err != nil {
		t.Fatal(err)
	}
	dest, _ := NewDirectory(64, 10, cache)
	sender, receiver := NewSynchronizer(source, source.cache), NewSynchronizer(dest, cache)
	msgs := sender.Reconcile(now)
	if len(msgs) != 2 || msgs[0].Kind != "bundle" || msgs[1].Kind != "manifest" {
		t.Fatal(msgs)
	}
	// Deliberately omit the proactively sent bundle.
	replies, changed, err := receiver.Receive(msgs[1], now)
	if err != nil || len(changed) != 0 || len(replies) != 1 || replies[0].Kind != "identity-request" {
		t.Fatal(replies, changed, err)
	}
	bundles, _, err := sender.Receive(replies[0], now)
	if err != nil || len(bundles) != 1 {
		t.Fatal(err)
	}
	_, changed, err = receiver.Receive(bundles[0], now)
	if err != nil || len(changed) != 1 || len(dest.Snapshot(now)) != 1 {
		t.Fatal("bundle recovery failed", err)
	}
	if len(sender.Reconcile(now)) != 1 {
		t.Fatal("certificate repeated unnecessarily")
	}
	_, changed, err = receiver.Receive(msgs[1], now)
	if err != nil || len(changed) != 0 {
		t.Fatal("duplicate re-flooded", err)
	}
}

func TestDirectorySyncWarmBundleHintAndReceiverRestart(t *testing.T) {
	source, chain, key, now := directoryFixture(t)
	m := Manifest{Version: 1, Org: 64, Asset: 445, Revision: 1, Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), AgentPort: 50052}
	w, err := SignManifest(m, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	cold := NewSynchronizer(source, source.cache).Reconcile(now)
	warmSender := NewSynchronizer(source, source.cache)
	warmSender.SeedKnownBundles([]string{w.Fingerprint})
	warm := warmSender.Reconcile(now)
	if len(cold) != 2 || cold[0].Kind != "bundle" || len(warm) != 1 || warm[0].Kind != "manifest" {
		t.Fatalf("cold=%v warm=%v", cold, warm)
	}
	var coldWire, warmWire bytes.Buffer
	for _, msg := range cold {
		if err := WriteControl(&coldWire, msg); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteControl(&warmWire, warm[0]); err != nil {
		t.Fatal(err)
	}
	if warmWire.Len() >= coldWire.Len() {
		t.Fatalf("hint did not save bytes: cold=%d warm=%d", coldWire.Len(), warmWire.Len())
	}
	// A peer may have restarted and lost its cache after our successful write.
	// Its request must repair the identity before the manifest is admitted.
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), source.cache.verify, now)
	if err != nil {
		t.Fatal(err)
	}
	dest, _ := NewDirectory(64, 10, cache)
	receiver := NewSynchronizer(dest, cache)
	replies, changed, err := receiver.Receive(warm[0], now)
	if err != nil || len(changed) != 0 || len(replies) != 1 || replies[0].Kind != "identity-request" {
		t.Fatalf("repair request: replies=%v changed=%v err=%v", replies, changed, err)
	}
	bundles, _, err := warmSender.Receive(replies[0], now)
	if err != nil || len(bundles) != 1 || bundles[0].Kind != "bundle" {
		t.Fatalf("repair bundle: %v %v", bundles, err)
	}
	_, changed, err = receiver.Receive(bundles[0], now)
	if err != nil || len(changed) != 1 || len(dest.Snapshot(now)) != 1 {
		t.Fatalf("repaired manifest not admitted: changed=%v err=%v", changed, err)
	}
	bad := w
	bad.Signature = append([]byte(nil), w.Signature...)
	bad.Signature[0] ^= 1
	if _, _, err = receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &bad}, now); err == nil {
		t.Fatal("hint bypassed signature validation")
	}
}

func TestDirectorySyncMissingIdentityBacklogIsBounded(t *testing.T) {
	source, chain, key, now := directoryFixture(t)
	m := Manifest{Version: 1, Org: 64, Asset: 445, Revision: 1, Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), AgentPort: 50052}
	w, err := SignManifest(m, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), source.cache.verify, now)
	if err != nil {
		t.Fatal(err)
	}
	dest, _ := NewDirectory(64, 10, cache)
	receiver := NewSynchronizer(dest, cache)
	for i := 0; i < 128; i++ {
		missing := w
		missing.Fingerprint = fmt.Sprintf("%064x", i)
		replies, changed, err := receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &missing}, now)
		if err != nil || len(changed) != 0 || len(replies) != 1 || replies[0].Kind != "identity-request" {
			t.Fatalf("identity %d: replies=%v changed=%v err=%v", i, replies, changed, err)
		}
	}
	replies, changed, err := receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, now)
	if err != nil || len(changed) != 0 || len(replies) != 1 || replies[0].Kind != "identity-request" {
		t.Fatalf("overflow repair request: replies=%v changed=%v err=%v", replies, changed, err)
	}
	if got := len(receiver.pending); got != 128 {
		t.Fatalf("pending identities=%d, want 128", got)
	}
	_, changed, err = receiver.Receive(ControlMessage{Kind: "bundle", Bundle: chain}, now)
	if err != nil || len(changed) != 0 {
		t.Fatalf("overflow bundle: changed=%v err=%v", changed, err)
	}
	_, changed, err = receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, now)
	if err != nil || len(changed) != 1 {
		t.Fatalf("replayed overflow manifest: changed=%v err=%v", changed, err)
	}
}
