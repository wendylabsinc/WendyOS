//go:build linux

package localmesh

import (
	"context"
	"crypto/ecdsa"
	"encoding/pem"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestLeafQueuedBundleRenewalUsesCurrentVerifiedChain(t *testing.T) {
	for _, supersede := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-key-current-wire", true: "superseded-wire"}[supersede], func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			old := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(time.Hour))
			renewed := newManifestTestAuthority(t, now).issue(t, now, old.Signer.(*ecdsa.PrivateKey), now.Add(2*time.Hour))
			var allowOld atomic.Bool
			allowOld.Store(true)
			verify := func(chain [][]byte, at time.Time) (Identity, error) {
				if id, err := renewed.Verify(chain, at); err == nil {
					return id, nil
				}
				if allowOld.Load() {
					return old.Verify(chain, at)
				}
				return Identity{}, errors.New("old leaf issuer retired")
			}
			cache, err := OpenIdentityCache("", DefaultCacheLimits(), verify, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cache.Put(old.Certificate.Certificate, now); err != nil {
				t.Fatal(err)
			}
			directory, err := NewDirectory(old.Device.Tenant(), 8, cache)
			if err != nil {
				t.Fatal(err)
			}
			manifest := routeManifest(old.Device, now)
			oldWire, err := SignManifest(manifest, old.Certificate.Certificate, old.Signer, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := directory.Accept(oldWire, now); err != nil {
				t.Fatal(err)
			}
			sync := newLeafSynchronizer(directory, cache)
			defer sync.Close()
			queued := sync.Record(oldWire, now)
			if len(queued) != 2 || queued[0].Kind != "bundle" {
				t.Fatal("missing leaf bundle/manifest pair")
			}
			link := &heldManifestControlLink{entered: make(chan struct{}), release: make(chan struct{}), written: make(chan ControlMessage, 8)}
			peer := testDevice(t, testPrincipal(testTenantUUID, testDeviceBID))
			p := &nodePeer{device: peer, link: link, control: make(chan ControlMessage, 8)}
			p.control <- ControlMessage{Kind: "identity-request", Principal: "barrier"}
			for _, message := range queued {
				p.control <- message
			}
			node := &Node{cache: cache, directory: directory, bundleHints: newBundleHintCache()}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- node.writeControls(ctx, p) }()
			t.Cleanup(func() {
				select {
				case <-link.release:
				default:
					close(link.release)
				}
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Error(err)
					}
				case <-time.After(time.Second):
					t.Error("leaf writer did not cancel")
				}
			})
			select {
			case <-link.entered:
			case <-time.After(time.Second):
				t.Fatal("writer did not reach barrier")
			}
			// Retire the queued bundle's issuer while an earlier transport write holds
			// the worker. The old DER is only a principal hint, never a trust source.
			allowOld.Store(false)
			if _, err := cache.Put(renewed.Certificate.Certificate, now); err != nil {
				t.Fatal(err)
			}
			wantWire := oldWire
			if supersede {
				manifest.Revision++
				current, err := SignManifest(manifest, renewed.Certificate.Certificate, renewed.Signer, now)
				if err != nil {
					t.Fatal(err)
				}
				if changed, err := directory.Accept(current, now); err != nil || !changed {
					t.Fatal(changed, err)
				}
				wantWire = current
				p.control <- ControlMessage{Kind: "manifest", Manifest: &current}
			}
			p.control <- ControlMessage{Kind: "identity-request", Principal: "end"}
			close(link.release)
			var sent []ControlMessage
			for {
				select {
				case message := <-link.written:
					if message.Principal == "barrier" {
						continue
					}
					if message.Principal == "end" {
						goto drained
					}
					sent = append(sent, message)
				case <-time.After(time.Second):
					t.Fatal("renewal stalled leaf control")
				}
			}
		drained:
			if len(sent) != 2 || sent[0].Kind != "bundle" || !sameChain(sent[0].Bundle, renewed.Certificate.Certificate) || sent[1].Manifest == nil || !reflect.DeepEqual(*sent[1].Manifest, wantWire) {
				t.Fatalf("leaf did not receive current chain/current proof (%d messages)", len(sent))
			}
			// A fresh leaf that trusts only the replacement issuer must authenticate
			// the transmitted proof without possessing any previous identity state.
			coldCache, err := OpenIdentityCache("", DefaultCacheLimits(), renewed.Verify, now)
			if err != nil {
				t.Fatal(err)
			}
			cold, err := NewDirectory(renewed.Device.Tenant(), 8, coldCache)
			if err != nil {
				t.Fatal(err)
			}
			receiver := newLeafSynchronizer(cold, coldCache)
			defer receiver.Close()
			if got := exchangeDirectoryMessages(t, sync, receiver, sent, now); len(got) != 1 || len(cold.Snapshot(now)) != 1 {
				t.Fatal("cold leaf could not verify renewed bundle/proof")
			}
			hints := node.bundleHints.Known(peer, time.Now())
			if len(hints) != 1 || hints[0] != Fingerprint(renewed.Certificate.Certificate) || hints[0] == Fingerprint(old.Certificate.Certificate) {
				t.Fatalf("Written recorded queued rather than transmitted bytes: %#v", hints)
			}
		})
	}
}

func TestLeafQueuedBundleHintsFailClosedWithoutChangingRequestedChunks(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	known := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(time.Hour))
	unknown := testCredentialsFor(t, testPrincipal(testTenantUUID, testDeviceBID))
	nonDevicePEM, _ := testIssue(t, "spiffe://wendy.sh/tenant/"+testTenantUUID+"/service/leaf-test")
	nonDevice, _ := pem.Decode([]byte(nonDevicePEM))
	if nonDevice == nil {
		t.Fatal("non-device fixture missing")
	}
	for _, tc := range []struct {
		name                  string
		bundle                [][]byte
		missingCache, retired bool
	}{
		{name: "empty"},
		{name: "malformed", bundle: [][]byte{[]byte("not DER")}},
		{name: "non-device", bundle: [][]byte{nonDevice.Bytes}},
		{name: "unknown", bundle: unknown.Certificate.Certificate},
		{name: "retired", bundle: known.Certificate.Certificate, retired: true},
		{name: "missing-cache", bundle: known.Certificate.Certificate, missingCache: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verify := func(chain [][]byte, at time.Time) (Identity, error) {
				if tc.retired {
					return Identity{}, errors.New("issuer retired")
				}
				return known.Verify(chain, at)
			}
			cache, err := OpenIdentityCache("", DefaultCacheLimits(), known.Verify, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cache.Put(known.Certificate.Certificate, now); err != nil {
				t.Fatal(err)
			}
			cache.verify = verify
			node := &Node{cache: cache, bundleHints: newBundleHintCache()}
			if tc.missingCache {
				node.cache = nil
			}
			// Explicit requested chunks are content-addressed protocol data. They
			// must keep their exact bytes even when a bundle hint is dropped.
			chunk := ControlMessage{Kind: "certificate", Certificate: &CertificateMessage{Chunk: &CertificateChunk{Hash: certificateHash(known.Certificate.Certificate[0]), Total: len(known.Certificate.Certificate[0]), Data: append([]byte(nil), known.Certificate.Certificate[0]...)}}}
			link := &heldManifestControlLink{entered: make(chan struct{}), release: make(chan struct{}), written: make(chan ControlMessage, 4)}
			p := &nodePeer{device: unknown.Device, link: link, control: make(chan ControlMessage, 4)}
			p.control <- ControlMessage{Kind: "bundle", Bundle: tc.bundle}
			p.control <- chunk
			p.control <- ControlMessage{Kind: "identity-request", Principal: "end"}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- node.writeControls(ctx, p) }()
			defer cancel()
			for _, want := range []ControlMessage{chunk, {Kind: "identity-request", Principal: "end"}} {
				select {
				case got := <-link.written:
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("unsafe bundle or changed chunk: kind=%s principal=%s", got.Kind, got.Principal)
					}
				case <-time.After(time.Second):
					t.Fatal("invalid hint blocked control progress")
				}
			}
			if hints := node.bundleHints.Known(p.device, time.Now()); len(hints) != 0 {
				t.Fatal("dropped hint recorded as written")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("writer did not cancel")
			}
		})
	}
}
