package meshcatalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

// A descriptor is a bounded repair hint from this link, never trust in its
// claimed leaf. Both real chains are trusted when renewal starts; the old
// certificate can expire while its bounded repair remains pending.
func TestWarmServiceRenewalRequiresExactPendingDescriptor(t *testing.T) {
	old, current := newFixtureWithOptions(t, 15*time.Second, nil), newFixture(t)
	old.now = current.now
	sameKey := newFixtureWithOptions(t, 24*time.Hour, old.creds)
	sameKey.now = old.now
	shortNew := newFixtureWithOptions(t, 30*time.Second, nil)
	shortNew.now = old.now
	origin, _ := old.newCatalog(t, p533, "default", nil, nil)
	initialSpec := testSpec()
	initialSpec.Lease = 10 * time.Second
	first, err := origin.Publish(initialSpec, old.now)
	if err != nil {
		t.Fatal(err)
	}
	key := Key{"default", p533, testSpec().AppID, testSpec().ServiceID}
	currentOrigin, currentCache := current.newCatalog(t, p533, "default", []Receipt{origin.history[key]}, nil)
	renewed, err := currentOrigin.Publish(testSpec(), old.now)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"exact", "missing", "wrong-leaf", "other-link", "expired", "replacement", "deadline", "bad-signature", "bad-chain", "bounds", "same-key", "lease-too-long", "bundle", "coalesced-principals"} {
		t.Run(mode, func(t *testing.T) {
			verify := func(chain [][]byte, now time.Time) (localmesh.Identity, error) {
				if id, err := current.creds[p534].Verify(chain, now); err == nil {
					return id, nil
				}
				if id, err := shortNew.creds[p534].Verify(chain, now); err == nil {
					return id, nil
				}
				if id, err := sameKey.creds[p534].Verify(chain, now); err == nil {
					return id, nil
				}
				return old.creds[p534].Verify(chain, now)
			}
			cache, err := localmesh.OpenIdentityCache("", localmesh.DefaultCacheLimits(), verify, old.now)
			if err != nil {
				t.Fatal(err)
			}
			dest, err := NewCatalog("default", old.creds[p534].Device, 32, old.creds[p534], cache, func(string, string, uint16) error { return nil }, nil, func([]Receipt) error { return nil }, old.now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cache.Put(old.creds[p533].Certificate.Certificate, old.now); err != nil {
				t.Fatal(err)
			}
			if _, err := dest.Accept(first, old.now); err != nil {
				t.Fatal(err)
			}
			sendCatalog, sendCache := currentOrigin, currentCache
			incoming := cloneWire(renewed)
			if mode == "same-key" {
				sendCatalog, sendCache = sameKey.newCatalog(t, p533, "default", []Receipt{origin.history[key]}, nil)
				incoming, err = sendCatalog.Publish(testSpec(), old.now)
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "lease-too-long" {
				sendCatalog, sendCache = shortNew.newCatalog(t, p533, "default", nil, nil)
				var record Record
				if err := json.Unmarshal(renewed.Body, &record); err != nil {
					t.Fatal(err)
				}
				incoming, err = Sign(record, shortNew.creds[p533].Certificate.Certificate, shortNew.creds[p533].Signer, old.now)
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "bad-chain" {
				untrusted := newFixture(t)
				untrusted.now = old.now
				sendCatalog, sendCache = untrusted.newCatalog(t, p533, "default", []Receipt{origin.history[key]}, nil)
				incoming, err = sendCatalog.Publish(testSpec(), old.now)
				if err != nil {
					t.Fatal(err)
				}
			}
			sender, receiver := NewSynchronizer(sendCatalog, sendCache), NewSynchronizer(dest, cache)
			defer sender.Close()
			defer receiver.Close()
			descriptor := localmesh.DescribeCertificates(p533, sendCatalog.creds.Certificate.Certificate)
			target, at := receiver, old.now
			if mode == "other-link" {
				target = NewSynchronizer(dest, cache)
				defer target.Close()
			}
			if mode == "expired" {
				at = at.Add(-localmesh.MaxLease)
			}
			if mode == "deadline" {
				at = at.Add(-localmesh.MaxLease + 30*time.Second)
			}
			if mode == "wrong-leaf" {
				descriptor.Chain.Hashes[0] = strings.Repeat("0", 64)
			}

			if mode == "bad-signature" {
				incoming.Signature[len(incoming.Signature)-1] ^= 1
			}
			var replies []Message
			if mode != "missing" {
				replies, _, err = target.Receive(Message{Kind: "certificate", Certificate: descriptor}, at)
				if err != nil {
					t.Fatal(err)
				}
			}
			out, changed, err := receiver.Receive(Message{Kind: "record", Record: &incoming}, old.now)
			if mode != "exact" && mode != "replacement" && mode != "deadline" && mode != "bad-signature" && mode != "bad-chain" && mode != "bounds" && mode != "same-key" && mode != "lease-too-long" && mode != "bundle" && mode != "coalesced-principals" {
				if err == nil || errors.Is(err, ErrIdentityNeeded) || receiver.PendingIdentity() {
					t.Fatalf("unbound mismatch deferred: err=%v pending=%v", err, receiver.PendingIdentity())
				}
				return
			}
			if err != nil || len(changed) != 0 || !receiver.PendingIdentity() {
				t.Fatalf("exact descriptor did not defer warm mismatch: changes=%d pending=%v err=%v", len(changed), receiver.PendingIdentity(), err)
			}
			if mode == "bundle" {
				_, changed, err := receiver.Receive(Message{Kind: "bundle", Bundle: sendCatalog.creds.Certificate.Certificate}, old.now)
				if err != nil || len(changed) != 0 || !receiver.PendingIdentity() {
					t.Fatal("bundle bypassed advertised exchange", err)
				}
				_, changed, err = receiver.ContinueCertificates(old.now)
				if err != nil || len(changed) != 1 || receiver.PendingIdentity() {
					t.Fatal("matching verified exchange did not complete after public bytes arrived", err)
				}
				return
			}
			if mode == "coalesced-principals" {
				if _, err := cache.Put(old.creds[p536].Certificate.Certificate, old.now); err != nil {
					t.Fatal(err)
				}
				other, _ := current.newCatalog(t, p536, "default", nil, nil)
				second, err := other.Publish(testSpec(), old.now)
				if err != nil {
					t.Fatal(err)
				}
				description := localmesh.DescribeCertificates(p536, current.creds[p536].Certificate.Certificate)
				if _, _, err := receiver.Receive(Message{Kind: "certificate", Certificate: description}, old.now); err != nil {
					t.Fatal(err)
				}
				if _, _, err := receiver.Receive(Message{Kind: "record", Record: &second}, old.now); err != nil {
					t.Fatal(err)
				}
				for _, principal := range []string{p533, p536} {
					if _, err := cache.Put(current.creds[principal].Certificate.Certificate, old.now); err != nil {
						t.Fatal(err)
					}
				}
				_, changed, err := receiver.ContinueCertificates(old.now)
				if err != nil || len(changed) != 2 || receiver.PendingIdentity() || len(dest.Snapshot(old.now)) != 2 {
					t.Fatalf("coalesced completion lost pending data: changes=%d err=%v", len(changed), err)
				}
				return
			}
			if mode == "bounds" {
				for i := 1; i <= maxPendingRecords; i++ {
					extra := cloneWire(incoming)
					extra.Signature = []byte{byte(i), byte(i >> 8)}
					_, _, err := receiver.Receive(Message{Kind: "record", Record: &extra}, old.now)
					if i == maxPendingRecords {
						if !errors.Is(err, errPendingIdentities) {
							t.Fatal("warm data escaped pending bound", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
				}
				return
			}
			if mode == "replacement" {
				replacement := localmesh.DescribeCertificates(p533, current.creds[p533].Certificate.Certificate)
				replacement.Chain.Hashes = append(replacement.Chain.Hashes, strings.Repeat("1", 64))
				if _, _, err := receiver.Receive(Message{Kind: "certificate", Certificate: replacement}, old.now); err != nil {
					t.Fatal(err)
				}
				if receiver.PendingIdentity() {
					t.Fatal("replacement retained data bound to another full descriptor")
				}
				return
			}
			if mode == "deadline" {
				// Repeating a descriptor/data after the old certificate expires cannot
				// downgrade this warm candidate into an unbound cold-cache retry.
				near := old.now.Add(29 * time.Second)
				if _, _, err := receiver.Receive(Message{Kind: "certificate", Certificate: descriptor}, near); err != nil {
					t.Fatal(err)
				}
				if _, _, err := receiver.Receive(Message{Kind: "record", Record: &incoming}, near); err != nil {
					t.Fatal(err)
				}
				changedWire := cloneWire(incoming)
				changedWire.Signature[0] ^= 1
				if _, _, err := receiver.Receive(Message{Kind: "record", Record: &changedWire}, near); err != nil {
					t.Fatal(err)
				}
				if _, _, err := receiver.ContinueCertificates(old.now.Add(30 * time.Second)); err != nil {
					t.Fatal(err)
				}
				if receiver.PendingIdentity() {
					t.Fatal("repeated descriptor extended pending record lifetime")
				}
				return
			}
			// Drain exactly the requested real DER elements through the public wire
			// codec, then admit only after full chain and record signature verification.
			var initial []Message
			for _, reply := range append(replies, out...) {
				messages, _, err := sender.Receive(reply, old.now)
				if err != nil {
					t.Fatal(err)
				}
				initial = append(initial, messages...)
			}
			changed, err = exchangeWarmServiceMessages(sender, receiver, initial, old.now)
			if mode == "bad-signature" || mode == "bad-chain" || mode == "lease-too-long" {
				if err == nil || len(changed) != 0 || dest.history[key].Generation != 1 {
					t.Fatalf("invalid repaired proof admitted: changes=%d err=%v", len(changed), err)
				}
				return
			}
			if err != nil || len(changed) != 1 {
				t.Fatalf("warm renewal did not recover: changes=%d err=%v", len(changed), err)
			}
			if receiver.PendingIdentity() || len(dest.Snapshot(old.now)) != 1 || dest.history[key].Generation != 2 {
				t.Fatal("renewal lost readiness or generation")
			}
			if _, err := dest.Accept(first, old.now); err == nil {
				t.Fatal("old artifact accepted after renewal")
			}
			if len(dest.Snapshot(old.now.Add(testSpec().Lease+time.Second))) != 0 || dest.history[key].Generation != 2 {
				t.Fatal("repair extended signed expiry or lost replay floor")
			}
		})
	}
}

// Exercise the real public framing while returning expected security failures.
func exchangeWarmServiceMessages(sender, receiver *Synchronizer, initial []Message, now time.Time) (changed []SignedRecord, err error) {
	type delivery struct {
		message    Message
		toReceiver bool
	}
	var queue []delivery
	for _, m := range initial {
		queue = append(queue, delivery{m, true})
	}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 10000 {
			return changed, errors.New("certificate exchange did not quiesce")
		}
		d := queue[0]
		queue = queue[1:]
		var wire bytes.Buffer
		if err := WriteMessage(&wire, d.message); err != nil {
			return changed, err
		}
		decoded, err := ReadMessage(&wire)
		if err != nil {
			return changed, err
		}
		target := sender
		if d.toReceiver {
			target = receiver
		}
		out, admitted, err := target.Receive(decoded, now)
		if err != nil {
			return changed, err
		}
		if d.toReceiver {
			changed = append(changed, admitted...)
		}
		for _, m := range out {
			queue = append(queue, delivery{m, !d.toReceiver})
		}
	}
	return changed, nil
}
