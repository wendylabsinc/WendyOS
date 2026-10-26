package localmesh

import (
	"bytes"
	"testing"
	"time"
)

// This regression also compiles against the former whole-bundle protocol:
// that sender's first frame is >32 KiB, before any certificate is requested.
func TestDirectoryCertificateExchangeAvoidsWholeChainFrames(t *testing.T) {
	a, b := largeChainCredentials(t, 6500)
	now := time.Now()
	sourceCache, err := OpenIdentityCache("", DefaultCacheLimits(), a.Verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sourceCache.Put(a.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	source, _ := NewDirectory(a.Device.Tenant(), 10, sourceCache)
	w, err := SignManifest(routeManifest(a.Device, now), a.Certificate.Certificate, a.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	destCache, err := OpenIdentityCache("", DefaultCacheLimits(), b.Verify, now)
	if err != nil {
		t.Fatal(err)
	}
	dest, _ := NewDirectory(b.Device.Tenant(), 10, destCache)
	sender, receiver := NewSynchronizer(source, sourceCache), NewSynchronizer(dest, destCache)
	type delivery struct {
		message    ControlMessage
		toReceiver bool
	}
	var queue []delivery
	for _, m := range sender.Reconcile(now) {
		queue = append(queue, delivery{m, true})
	}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 1000 {
			t.Fatal("certificate fetch did not converge")
		}
		d := queue[0]
		queue = queue[1:]
		var wire bytes.Buffer
		if err := WriteControl(&wire, d.message); err != nil {
			t.Fatal(err)
		}
		if wire.Len() > 1800 {
			t.Fatalf("directory sent a %d-byte %s frame; certificate bytes must be requested in bounded chunks", wire.Len(), d.message.Kind)
		}
		decoded, err := ReadControl(&wire)
		if err != nil {
			t.Fatal(err)
		}
		target := sender
		if d.toReceiver {
			target = receiver
		}
		replies, _, err := target.Receive(decoded, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, reply := range replies {
			queue = append(queue, delivery{reply, !d.toReceiver})
		}
	}
	if len(dest.Snapshot(now)) != 1 {
		t.Fatal("bounded element fetch did not authenticate and admit the signed manifest")
	}
}
