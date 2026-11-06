package localmesh

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestControlBoundAndFraming(t *testing.T) {
	var b bytes.Buffer
	m := ControlMessage{Kind: "identity-request", Principal: testPrincipal(testTenantUUID, testDeviceAID)}
	if err := WriteControl(&b, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadControl(&b)
	if err != nil || got.Kind != m.Kind || got.Principal != m.Principal {
		t.Fatal(got, err)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], MaxControlMessage+1)
	if _, err = ReadControl(bytes.NewReader(header[:])); err == nil {
		t.Fatal("oversize allocated")
	}
}

func TestDirectorySyncBundleOnceAndMissingBundleRecovery(t *testing.T) {
	source, chain, key, device, now := directoryFixture(t)
	m := routeManifest(device, now)
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
	dest, _ := NewDirectory(testTenant(t), 10, cache)
	sender, receiver := NewSynchronizer(source, source.cache), NewSynchronizer(dest, cache)
	msgs := sender.Reconcile(now)
	if len(msgs) != 2 || msgs[0].Kind != "certificate" || msgs[1].Kind != "manifest" {
		t.Fatal(msgs)
	}
	// Omit the descriptor to exercise principal-scoped repair.
	replies, changed, err := receiver.Receive(msgs[1], now)
	if err != nil || len(changed) != 0 || len(replies) != 1 || replies[0].Kind != "identity-request" {
		t.Fatal(replies, changed, err)
	}
	bundles, _, err := sender.Receive(replies[0], now)
	if err != nil || len(bundles) != 1 {
		t.Fatal(err)
	}
	changed = exchangeDirectoryMessages(t, sender, receiver, bundles, now)
	if len(changed) != 1 || len(dest.Snapshot(now)) != 1 {
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

// TestSyncIdentityRequestIsPrincipalScoped covers the two ways a neighbour can
// ask for a certificate: a principal this node holds is answered, anything
// else - an unknown device, or the same device UUID under another tenant - is
// not, and a manifest from another tenant ends the link rather than being
// relayed as a hint.
func TestSyncIdentityRequestIsPrincipalScoped(t *testing.T) {
	source, chain, key, device, now := directoryFixture(t)
	w, err := SignManifest(routeManifest(device, now), chain, key, now)
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
	dest, _ := NewDirectory(testTenant(t), 10, cache)
	sender, receiver := NewSynchronizer(source, source.cache), NewSynchronizer(dest, cache)
	replies, _, err := receiver.Receive(ControlMessage{Kind: "identity-request", Principal: device.Principal()}, now)
	if err != nil || len(replies) != 1 || replies[0].Kind != "identity-missing" {
		t.Fatal("receiver did not report its missing identity", replies, err)
	}
	replies, _, err = sender.Receive(ControlMessage{Kind: "identity-request", Principal: device.Principal()}, now)
	if err != nil || len(replies) != 1 || replies[0].Kind != "certificate" {
		t.Fatal("known principal unanswered", err)
	}
	replies, _, err = sender.Receive(ControlMessage{Kind: "identity-request", Principal: testPrincipal(testOtherTenant, testDeviceAID)}, now)
	if err != nil || len(replies) != 0 {
		t.Fatal("principal outside the tenant was answered", err)
	}
	foreign := w
	foreign.Principal = testPrincipal(testOtherTenant, testDeviceAID)
	if _, _, err = receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &foreign}, now); err == nil {
		t.Fatal("manifest from another tenant relayed without error")
	}
}

func TestBoundedReconcileDoesNotConsumeUnqueuedBundleHint(t *testing.T) {
	source, chain, key, device, now := directoryFixture(t)
	w, err := SignManifest(routeManifest(device, now), chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	sender := NewSynchronizer(source, source.cache)
	if got := sender.ReconcileBounded(now, 1); len(got) != 0 {
		t.Fatal("split a bundle and its manifest across an insufficient queue budget")
	}
	got := sender.ContinueReconcile(now, 2)
	if len(got) != 2 || got[0].Kind != "certificate" || got[1].Kind != "manifest" {
		t.Fatalf("unqueued bundle was suppressed: %#v", got)
	}
	if got := sender.ContinueReconcile(now, 2); len(got) != 0 {
		t.Fatal("finished repair pass repeated records")
	}
}

// Drive the real bidirectional protocol to quiescence, including multiple
// certificate chunks. A received reply may itself require a response.
func exchangeDirectoryMessages(t *testing.T, sender, receiver *Synchronizer, initial []ControlMessage, now time.Time) []SignedManifest {
	t.Helper()
	type delivery struct {
		message    ControlMessage
		toReceiver bool
	}
	var queue []delivery
	for _, m := range initial {
		queue = append(queue, delivery{m, true})
	}
	var changed []SignedManifest
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 100000 {
			t.Fatal("directory exchange did not quiesce")
		}
		d := queue[0]
		queue = queue[1:]
		target := sender
		if d.toReceiver {
			target = receiver
		}
		var wire bytes.Buffer
		if err := WriteControl(&wire, d.message); err != nil {
			t.Fatal(err)
		}
		decoded, err := ReadControl(&wire)
		if err != nil {
			t.Fatal(err)
		}
		replies, admitted, err := target.Receive(decoded, now)
		if err != nil {
			t.Fatal(err)
		}
		if d.toReceiver {
			changed = append(changed, admitted...)
		}
		for _, reply := range replies {
			queue = append(queue, delivery{reply, !d.toReceiver})
		}
	}
	return changed
}
