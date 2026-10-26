package meshcatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestQueuedDirectCatalogueRenewalRechecksBeforeHubSend(t *testing.T) {
	for _, keepOld := range []bool{false, true} {
		name := "retired-old-issuer"
		if keepOld {
			name = "trusted-old-issuer"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				old, current := newFixture(t), newFixture(t)
				old.now = current.now
				allowOld := true
				verify := func(chain [][]byte, at time.Time) (localmesh.Identity, error) {
					if id, err := current.creds[p534].Verify(chain, at); err == nil {
						return id, nil
					}
					if allowOld {
						return old.creds[p534].Verify(chain, at)
					}
					return localmesh.Identity{}, errors.New("old issuer retired")
				}
				makeRelay := func(principal string) (*Catalog, *localmesh.IdentityCache) {
					cache, err := localmesh.OpenIdentityCache("", localmesh.DefaultCacheLimits(), verify, old.now)
					if err != nil {
						t.Fatal(err)
					}
					c, err := NewCatalog("default", old.creds[principal].Device, 32, old.creds[principal], cache, func(string, string, uint16) error { return nil }, nil, func([]Receipt) error { return nil }, old.now)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := cache.Put(old.creds[p533].Certificate.Certificate, old.now); err != nil {
						t.Fatal(err)
					}
					return c, cache
				}
				relay, cache := makeRelay(p534)
				warm, warmCache := makeRelay(p535)
				origin, _ := old.newCatalog(t, p533, "default", nil, nil)
				obsolete, err := origin.Publish(testSpec(), old.now)
				if err != nil {
					t.Fatal(err)
				}
				for _, c := range []*Catalog{relay, warm} {
					if _, err := c.Accept(obsolete, old.now); err != nil {
						t.Fatal(err)
					}
				}
				runtime, err := NewRuntime(relay, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
				if err != nil {
					t.Fatal(err)
				}
				hub, remote := localmesh.NewControlHub(), localmesh.NewControlHub()
				peer := localmesh.ControlPeer{ID: 1, Device: warm.device}
				remote.Add(peer, nil, func() {})
				<-remote.Events()
				entered, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
				first := true
				hub.Add(peer, func(ctx context.Context, frame []byte) error {
					if first {
						first = false
						close(entered)
						select {
						case <-release:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					return remote.Deliver(peer.ID, frame)
				}, func() { closed <- struct{}{} })
				runtime.hub = hub
				ctx, cancel := context.WithCancel(context.Background())
				runtime.syncPeers(ctx)
				p := runtime.peers[peer.ID]
				// This fixture targets the pre-encoded direct catalogue queue.
				// Router peers use the separately tested pooled HAVE/WANT path.
				p.syncer.Close()
				p.syncer = NewSynchronizer(relay, cache)
				defer func() { cancel(); synctest.Wait(); p.syncer.Close() }()
				if !runtime.queue(p, inventoryMessage{Message: Message{Kind: "identity-request", Principal: p534}}) {
					t.Fatal("barrier queue")
				}
				<-entered
				if !runtime.queueMessages(p, p.syncer.Record(obsolete, old.now)) {
					t.Fatal("old certificate/record queue")
				}
				// Both encoded messages are genuinely queued before renewal, while
				// an unrelated logical message is already in flight through hub.Send.
				allowOld = keepOld
				if _, err := cache.Put(current.creds[p533].Certificate.Certificate, old.now); err != nil {
					t.Fatal(err)
				}
				key := Key{"default", p533, testSpec().AppID, testSpec().ServiceID}
				currentOrigin, _ := current.newCatalog(t, p533, "default", []Receipt{relay.history[key]}, nil)
				admitted, err := currentOrigin.Publish(testSpec(), old.now)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := relay.Accept(admitted, old.now); err != nil {
					t.Fatal(err)
				}
				if !runtime.queueMessages(p, p.syncer.Record(admitted, old.now)) {
					t.Fatal("current certificate/record queue")
				}
				if !runtime.queue(p, inventoryMessage{Message: Message{Kind: "identity-request", Principal: p536}}) {
					t.Fatal("end queue")
				}
				close(release)
				synctest.Wait()
				var messages []Message
				records, descriptions := 0, 0
				for len(remote.Events()) > 0 {
					event := <-remote.Events()
					var message inventoryMessage
					if err := json.Unmarshal(event.Data, &message); err != nil {
						t.Fatal(err)
					}
					if message.Kind == "identity-request" {
						continue
					}
					if message.Certificate != nil && message.Certificate.Chain != nil {
						descriptions++
						if message.Certificate.Chain.Hashes[0] != admitted.Fingerprint {
							t.Error("queued descriptor still names the retired certificate")
						}
					}
					if message.Record != nil {
						records++
						if !bytes.Equal(message.Record.Signature, admitted.Signature) || message.Record.Fingerprint != admitted.Fingerprint {
							t.Error("queued old service artifact survived renewal")
						}
					}
					messages = append(messages, message.Message)
				}
				if records != 1 || descriptions != 2 || p.queued.Load() != 0 {
					t.Fatalf("bad dequeue progress/accounting: records=%d descriptions=%d queued=%d", records, descriptions, p.queued.Load())
				}
				select {
				case <-closed:
					t.Fatal("renewal closed a healthy neighbour")
				default:
				}
				sender := NewSynchronizer(relay, cache)
				receiver := NewSynchronizer(warm, warmCache)
				defer sender.Close()
				defer receiver.Close()
				if changed := exchangeServiceMessages(t, sender, receiver, messages, old.now); len(changed) != 1 {
					t.Fatal("warm receiver did not repair to current generation")
				}
				if got := warm.Snapshot(old.now); len(got) != 1 || got[0].Generation != 2 {
					t.Fatal("warm generation did not recover", got)
				}
				if len(warm.Snapshot(old.now.Add(testSpec().Lease+time.Second))) != 0 || warm.history[key].Generation != 2 {
					t.Fatal("queued renewal changed expiry or replay floor")
				}
			})
		})
	}
}

func TestDirectCatalogueDequeueRequiresExactCurrentArtifact(t *testing.T) {
	f := newFixture(t)
	catalog, _ := f.newCatalog(t, p533, "default", nil, nil)
	runtime, err := NewRuntime(catalog, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	if err != nil {
		t.Fatal(err)
	}
	wire, err := catalog.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(w SignedRecord) []byte {
		t.Helper()
		data, err := inventoryWire(inventoryMessage{Message: Message{Kind: "record", Record: &w}})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	check := func(w SignedRecord, at time.Time, want bool) {
		t.Helper()
		input := encode(w)
		got, err := runtime.prepareDirectControl(input, at)
		if err != nil || (got != nil) != want || want && !bytes.Equal(got, input) {
			t.Fatalf("dequeue: current=%v want=%v err=%v", got != nil, want, err)
		}
	}
	check(wire, f.now, true)
	changed := cloneWire(wire)
	changed.Signature[0] ^= 1
	check(changed, f.now, false)
	check(wire, f.now.Add(testSpec().Lease+time.Second), false)
	newer, err := catalog.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	check(wire, f.now, false)
	check(newer, f.now, true)
	withdrawal, err := catalog.Remove(testSpec().AppID, testSpec().ServiceID, f.now)
	if err != nil {
		t.Fatal(err)
	}
	check(newer, f.now, false)
	check(withdrawal, f.now, true)
	chain, ok := catalog.CurrentRecordChain(withdrawal, f.now)
	if !ok {
		t.Fatal("missing owned chain")
	}
	chain[0][0] ^= 1
	if _, ok := catalog.CurrentRecordChain(withdrawal, f.now); !ok {
		t.Fatal("returned chain aliases authoritative cache")
	}
	// Requested public certificate bytes and later content-addressed protocols
	// keep their exact admitted logical bytes through this legacy-only guard.
	request := inventoryMessage{Message: Message{Kind: "certificate", Certificate: &localmesh.CertificateMessage{Request: &localmesh.CertificateRequest{Hash: wire.Fingerprint}}}}
	data, err := inventoryWire(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, opaque := range [][]byte{data, []byte(`{"kind":"gossip","payload":"opaque future content"}`)} {
		got, err := runtime.prepareDirectControl(opaque, f.now)
		if err != nil || !bytes.Equal(got, opaque) {
			t.Fatal("unrelated logical bytes changed", err)
		}
	}
	if _, err := runtime.prepareDirectControl([]byte("invalid JSON"), f.now); err == nil {
		t.Fatal("invalid local queue silently bypassed guard")
	}
}
