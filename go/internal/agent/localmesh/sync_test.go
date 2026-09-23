package localmesh

import (
	"bytes"
	"encoding/binary"
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
