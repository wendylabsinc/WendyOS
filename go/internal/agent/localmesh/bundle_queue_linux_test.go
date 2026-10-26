//go:build linux

package localmesh

import (
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

type bundleGateLink struct {
	*teardownTestLink
	entered chan struct{}
	release chan struct{}
}

func (l *bundleGateLink) WriteControl(ControlMessage) error {
	close(l.entered)
	<-l.release
	return nil
}

func TestPendingIdentityRepairDoesNotFloodSlowLink(t *testing.T) {
	self, peer := testCredentials(t)
	link := &bundleGateLink{teardownTestLink: &teardownTestLink{done: make(chan struct{})}, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-link.release:
		default:
			close(link.release)
		}
	})
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), self.Verify, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Put(self.Certificate.Certificate, time.Now()); err != nil {
		t.Fatal(err)
	}
	directory, err := NewDirectory(self.Device.Tenant(), 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	n := &Node{cache: cache, bundleHints: newBundleHintCache()}
	p := &nodePeer{device: peer.Device, link: link, control: make(chan ControlMessage, 4)}
	p.sync = newLeafSynchronizer(directory, cache)
	defer p.sync.Close()
	request := func() []ControlMessage {
		replies, changed, err := p.sync.Receive(ControlMessage{Kind: "identity-request", Principal: self.Device.Principal()}, time.Now())
		if err != nil || len(changed) != 0 || len(replies) != 1 || replies[0].Kind != "bundle" {
			t.Fatalf("leaf identity reply: replies=%v changed=%v err=%v", replies, changed, err)
		}
		return replies
	}
	bundle := ControlMessage{Kind: "bundle", Bundle: self.Certificate.Certificate}
	if !n.queueReplies(p, request()) {
		t.Fatal("initial requested bundle refused")
	}
	writing := <-p.control
	done := make(chan error, 1)
	go func() { done <- n.writePeerControl(p, writing) }()
	select {
	case <-link.entered:
	case err := <-done:
		t.Fatalf("trusted bundle did not reach transport: %v", err)
	case <-time.After(time.Second):
		t.Fatal("trusted bundle writer stalled")
	}
	for i := 0; i < 100; i++ {
		if !n.queueReplies(p, request()) {
			t.Fatal("duplicate identity requests filled the requested reply reserve")
		}
		n.queue(p, []ControlMessage{bundle})
	}
	if len(p.control) != 0 {
		t.Fatal("duplicate certificate copies accumulated during the write")
	}
	select {
	case <-link.done:
		t.Fatal("repeated cache repair closed the link")
	default:
	}
	n.queue(p, []ControlMessage{{Kind: "manifest"}})
	if (<-p.control).Kind != "manifest" {
		t.Fatal("cache repair displaced a route proof")
	}
	close(link.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(n.bundleHints.Known(peer.Device, time.Now())) != 1 {
		t.Fatal("completed write was not recorded")
	}
	// A later request can represent a leaf reboot and must repair it again.
	if !n.queueReplies(p, request()) || len(p.control) != 1 {
		t.Fatal("completed bundle suppressed a later cache-repair request")
	}
}

type bundleCaptureLink struct {
	*teardownTestLink
	written []ControlMessage
	err     error
}

func (l *bundleCaptureLink) WriteControl(m ControlMessage) error {
	l.written = append(l.written, m)
	return l.err
}

func TestPendingBundleRenewalClearsOnlyOriginalToken(t *testing.T) {
	for _, sameKey := range []bool{false, true} {
		t.Run(map[bool]string{false: "new key", true: "same key"}[sameKey], func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			old := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(time.Hour))
			var key *ecdsa.PrivateKey
			if sameKey {
				key = old.Signer.(*ecdsa.PrivateKey)
			}
			renewed := newManifestTestAuthority(t, now).issue(t, now, key, now.Add(2*time.Hour))
			verify := func(chain [][]byte, at time.Time) (Identity, error) {
				if id, err := renewed.Verify(chain, at); err == nil {
					return id, nil
				}
				return old.Verify(chain, at)
			}
			cache, err := OpenIdentityCache("", DefaultCacheLimits(), verify, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cache.Put(old.Certificate.Certificate, now); err != nil {
				t.Fatal(err)
			}
			link := &bundleCaptureLink{teardownTestLink: &teardownTestLink{done: make(chan struct{})}}
			n := &Node{cache: cache, bundleHints: newBundleHintCache()}
			p := &nodePeer{device: old.Device, link: link, control: make(chan ControlMessage, 4)}
			n.queue(p, []ControlMessage{{Kind: "bundle", Bundle: old.Certificate.Certificate}})
			if _, err := cache.Put(renewed.Certificate.Certificate, now); err != nil {
				t.Fatal(err)
			}
			n.queue(p, []ControlMessage{{Kind: "bundle", Bundle: renewed.Certificate.Certificate}})
			if err := n.writePeerControl(p, <-p.control); err != nil {
				t.Fatal(err)
			}
			if len(link.written) != 1 || !sameChain(link.written[0].Bundle, renewed.Certificate.Certificate) {
				t.Fatal("queued bundle did not normalize to current trust")
			}
			if _, pending := p.pendingBundles.Load(Fingerprint(old.Certificate.Certificate)); pending {
				t.Fatal("normalization retained the original queued token")
			}
			if _, pending := p.pendingBundles.Load(Fingerprint(renewed.Certificate.Certificate)); !pending {
				t.Fatal("normalization removed another queued bundle's token")
			}
			known := n.bundleHints.Known(p.device, time.Now())
			if len(known) != 1 || known[0] != Fingerprint(renewed.Certificate.Certificate) {
				t.Fatal("hint did not record the actual transmitted current bundle")
			}
			if err := n.writePeerControl(p, <-p.control); err != nil {
				t.Fatal(err)
			}
			if _, pending := p.pendingBundles.Load(Fingerprint(renewed.Certificate.Certificate)); pending {
				t.Fatal("current bundle retained its token after writing")
			}
		})
	}
}

func TestPendingBundleDropsAndWriteErrorsReleaseToken(t *testing.T) {
	self, _ := testCredentials(t)
	for _, kind := range []string{"missing cache", "untrusted principal", "malformed certificate", "empty bundle", "write error"} {
		t.Run(kind, func(t *testing.T) {
			cache, err := OpenIdentityCache("", DefaultCacheLimits(), self.Verify, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			bundle := self.Certificate.Certificate
			link := &bundleCaptureLink{teardownTestLink: &teardownTestLink{done: make(chan struct{})}}
			n := &Node{cache: cache, bundleHints: newBundleHintCache()}
			switch kind {
			case "missing cache":
				n.cache = nil
			case "malformed certificate":
				bundle = [][]byte{[]byte("invalid certificate")}
			case "empty bundle":
				bundle = nil
			case "write error":
				if _, err := cache.Put(bundle, time.Now()); err != nil {
					t.Fatal(err)
				}
				link.err = errors.New("transport closed")
			}
			p := &nodePeer{device: self.Device, link: link, control: make(chan ControlMessage, 1)}
			message := ControlMessage{Kind: "bundle", Bundle: bundle}
			n.queue(p, []ControlMessage{message})
			if err := n.writePeerControl(p, <-p.control); !errors.Is(err, link.err) {
				t.Fatalf("write result = %v, want %v", err, link.err)
			}
			if _, pending := p.pendingBundles.Load(Fingerprint(bundle)); pending {
				t.Fatal("dropped or failed bundle retained its queued token")
			}
			if kind != "write error" && len(link.written) != 0 {
				t.Fatal("ineligible bundle reached the transport")
			}
			if len(n.bundleHints.Known(p.device, time.Now())) != 0 {
				t.Fatal("dropped or failed bundle created a written hint")
			}
			n.queue(p, []ControlMessage{message})
			if len(p.control) != 1 {
				t.Fatal("released bundle cannot be requested again")
			}
		})
	}
}

func TestPeerControlKeepsRequestedCertificateBytesIndependent(t *testing.T) {
	data := []byte("public certificate chunk")
	hash := certificateHash(data)
	messages := []ControlMessage{
		{Kind: "certificate", Certificate: &CertificateMessage{Request: &CertificateRequest{Hash: hash}}},
		{Kind: "certificate", Certificate: &CertificateMessage{Chunk: &CertificateChunk{Hash: hash, Total: len(data), Data: data}}},
	}
	link := &bundleCaptureLink{teardownTestLink: &teardownTestLink{done: make(chan struct{})}}
	p := &nodePeer{link: link, control: make(chan ControlMessage, 4)}
	p.pendingBundles.Store(hash, struct{}{})
	n := &Node{} // Public content-addressed chunks do not require current trust.
	messages = append(messages, messages...)
	if !n.queueReplies(p, messages) || len(p.control) != len(messages) {
		t.Fatal("requested certificate traffic was coalesced as identity bundles")
	}
	for range messages {
		if err := n.writePeerControl(p, <-p.control); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(link.written, messages) {
		t.Fatal("certificate request or chunk changed at the bundle export guard")
	}
	if _, pending := p.pendingBundles.Load(hash); !pending {
		t.Fatal("public certificate traffic removed an unrelated bundle token")
	}
}

func TestRequestedBundlesShareTokensAcrossControlAndReserve(t *testing.T) {
	self, peer := testCredentials(t)
	n := &Node{}
	p := &nodePeer{control: make(chan ControlMessage, 1)}
	first := ControlMessage{Kind: "bundle", Bundle: self.Certificate.Certificate}
	second := ControlMessage{Kind: "bundle", Bundle: peer.Certificate.Certificate}
	n.queue(p, []ControlMessage{first})
	if !n.queueReplies(p, []ControlMessage{first, second, second}) || len(p.control) != 1 || len(p.replies) != 1 {
		t.Fatal("requested bundles duplicated a queued frame or reserve entry")
	}
	if _, pending := p.pendingBundles.Load(Fingerprint(second.Bundle)); !pending {
		t.Fatal("reserved bundle does not own its token")
	}
	if err := n.writePeerControl(p, <-p.control); err != nil {
		t.Fatal(err)
	}
	n.queue(p, []ControlMessage{second}) // Flush the reserved frame first.
	if len(p.control) != 1 || len(p.replies) != 0 || p.replyBytes != 0 {
		t.Fatal("reserved bundle did not drain with correct byte accounting")
	}
	if !n.queueReplies(p, []ControlMessage{second}) || len(p.replies) != 0 {
		t.Fatal("requested bundle duplicated a frame flushed from the reserve")
	}
	if err := n.writePeerControl(p, <-p.control); err != nil {
		t.Fatal(err)
	}
	if !n.queueReplies(p, []ControlMessage{second}) || len(p.control) != 1 {
		t.Fatal("completed or dropped requested repair could not be retried")
	}
}

func TestRejectedReplyBatchDoesNotOwnBundleTokens(t *testing.T) {
	self, _ := testCredentials(t)
	bundle := ControlMessage{Kind: "bundle", Bundle: self.Certificate.Certificate}
	for _, failure := range []string{"frame limit", "byte limit", "invalid JSON"} {
		t.Run(failure, func(t *testing.T) {
			n := &Node{}
			p := &nodePeer{control: make(chan ControlMessage, 1)}
			p.control <- ControlMessage{Kind: "identity-request", Principal: "blocked"}
			var batch []ControlMessage
			switch failure {
			case "frame limit":
				for range maxDirectoryReplyFrames - 1 {
					if !n.queueReplies(p, []ControlMessage{{Kind: "identity-request"}}) {
						t.Fatal("fixture reserve filled too early")
					}
				}
				batch = []ControlMessage{bundle, {Kind: "identity-request"}}
			case "byte limit":
				// Serialized byte overload must reject the entire batch even
				// when its frame count remains below the limit.
				batch = []ControlMessage{bundle, {Kind: "certificate", Certificate: &CertificateMessage{Chunk: &CertificateChunk{Data: make([]byte, maxDirectoryReplyBytes)}}}}
			case "invalid JSON":
				batch = []ControlMessage{bundle, {Kind: "manifest", Manifest: &SignedManifest{Body: json.RawMessage("{")}}}
			}
			beforeFrames, beforeBytes := len(p.replies), p.replyBytes
			if n.queueReplies(p, batch) {
				t.Fatal("invalid or oversized batch admitted")
			}
			if len(p.replies) != beforeFrames || p.replyBytes != beforeBytes {
				t.Fatal("rejected batch changed the retained reply reserve")
			}
			if _, pending := p.pendingBundles.Load(Fingerprint(bundle.Bundle)); pending {
				t.Fatal("rejected batch left a bundle admission token")
			}
			if !n.queueReplies(p, []ControlMessage{bundle}) {
				t.Fatal("rejected batch suppressed a later valid bundle repair")
			}
			if _, pending := p.pendingBundles.Load(Fingerprint(bundle.Bundle)); !pending {
				t.Fatal("accepted retry did not own its bundle token")
			}
		})
	}
}

func TestPendingBundlesAreScopedToLinkAndCertificate(t *testing.T) {
	self, peer := testCredentials(t)
	n := &Node{bundleHints: newBundleHintCache()}
	makePeer := func() *nodePeer {
		return &nodePeer{device: peer.Device, control: make(chan ControlMessage, 4), link: &teardownTestLink{done: make(chan struct{})}}
	}
	first, second := makePeer(), makePeer()
	bundle := ControlMessage{Kind: "bundle", Bundle: self.Certificate.Certificate}
	n.queue(first, []ControlMessage{bundle, bundle})
	n.queue(second, []ControlMessage{bundle})
	n.queue(first, []ControlMessage{{Kind: "bundle", Bundle: peer.Certificate.Certificate}})
	if len(first.control) != 2 || len(second.control) != 1 {
		t.Fatal("deduplication crossed a certificate or link boundary")
	}
}

func TestDroppedBundleCanBeRequestedAgain(t *testing.T) {
	self, peer := testCredentials(t)
	n := &Node{bundleHints: newBundleHintCache()}
	p := &nodePeer{device: peer.Device, control: make(chan ControlMessage, 1), link: &teardownTestLink{done: make(chan struct{})}}
	p.control <- ControlMessage{Kind: "manifest"}
	bundle := ControlMessage{Kind: "bundle", Bundle: self.Certificate.Certificate}
	n.queue(p, []ControlMessage{bundle})
	<-p.control
	n.queue(p, []ControlMessage{bundle})
	if len(p.control) != 1 {
		t.Fatal("a full control queue permanently suppressed identity repair")
	}
}
