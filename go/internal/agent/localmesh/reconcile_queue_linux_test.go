//go:build linux

package localmesh

import (
	"fmt"
	"testing"
	"time"
)

// Reconnect must eventually transfer a directory larger than one link queue,
// including the certificate request/response repair for a restarted receiver.
func TestDirectoryReconciliationProgressBeyondLinkQueue(t *testing.T) {
	self, _ := testCredentials(t)
	now := time.Now()
	const origins = 132
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), self.Verify, now)
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewDirectory(self.Device.Tenant(), origins, cache)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < origins; i++ {
		principal := testPrincipal(testTenantUUID, fmt.Sprintf("2b1c0000-0000-4000-8000-%012x", i+10000))
		creds := testCredentialsFor(t, principal)
		if _, err := cache.Put(creds.Certificate.Certificate, now); err != nil {
			t.Fatal(err)
		}
		w, err := SignManifest(routeManifest(creds.Device, now), creds.Certificate.Certificate, creds.Signer, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := source.Accept(w, now); err != nil {
			t.Fatal(err)
		}
	}
	destCache, err := OpenIdentityCache("", DefaultCacheLimits(), self.Verify, now)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := NewDirectory(self.Device.Tenant(), origins, destCache)
	if err != nil {
		t.Fatal(err)
	}
	sender, receiver := NewSynchronizer(source, cache), NewSynchronizer(dest, destCache)
	node := &Node{}
	peer := &nodePeer{control: make(chan ControlMessage, 128), sync: sender}
	node.reconcilePeer(peer, now)
	for round := 0; round < 6; round++ {
		for len(peer.control) > 0 {
			message := <-peer.control
			exchangeDirectoryMessages(t, sender, receiver, []ControlMessage{message}, now)
		}
		node.continueReconcilePeer(peer, now)
	}
	if len(peer.control) != 0 {
		t.Fatal("completed repair pass kept retransmitting records")
	}
	if got := len(dest.Snapshot(now)); got != origins {
		t.Fatalf("reconciliation repeatedly starved directory tail: received %d of %d origins", got, origins)
	}
}
