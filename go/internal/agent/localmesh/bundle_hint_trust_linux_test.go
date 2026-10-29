//go:build linux

package localmesh

import (
	"context"
	"encoding/pem"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestQueuedBundleHintsFailClosedWithoutChangingRequestedChunks(t *testing.T) {
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
