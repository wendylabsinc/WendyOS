package localmesh

import (
	"bytes"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"testing"
	"time"
)

func TestDirectoryMissingRepliesKeepControlStreamUsable(t *testing.T) {
	source, _, _, device, now := directoryFixture(t)
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), source.cache.verify, now)
	if err != nil {
		t.Fatal(err)
	}
	directory, _ := NewDirectory(testTenant(t), 10, cache)
	empty := NewSynchronizer(directory, cache, netgossip.NewPool())
	defer empty.Close()
	for _, kind := range []string{"identity", "manifest"} {
		out, changed, err := empty.Receive(ControlMessage{Kind: kind + "-request", Principal: device.Principal()}, now, 2)
		if err != nil || len(out) != 1 || len(changed) != 0 || out[0].Kind != kind+"-missing" || out[0].Principal != device.Principal() {
			t.Fatal(out, changed, err)
		}
		var stream bytes.Buffer
		if err = WriteControl(&stream, out[0]); err != nil {
			t.Fatal(err)
		}
		if err = WriteControl(&stream, ControlMessage{Kind: "identity-request", Principal: device.Principal()}); err != nil {
			t.Fatal(err)
		}
		decoded, err := ReadControl(&stream)
		if err != nil || decoded.Kind != kind+"-missing" {
			t.Fatal(decoded, err)
		}
		if replies, changed, err := empty.Receive(decoded, now, 0); err != nil || len(replies) != 0 || len(changed) != 0 {
			t.Fatal(replies, changed, err)
		}
		next, err := ReadControl(&stream)
		if err != nil || next.Kind != "identity-request" {
			t.Fatal(next, err)
		}
	}
}

func TestIdentityDescriptorMissingTransfersToAlternateSupplier(t *testing.T) {
	source, chain, key, device, now := directoryFixture(t)
	wire, err := SignManifest(routeManifest(device, now), chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := netwire.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), source.cache.verify, now)
	if err != nil {
		t.Fatal(err)
	}
	directory, _ := NewDirectory(testTenant(t), 10, cache)
	pool := netgossip.NewPool()
	first, other := NewSynchronizer(directory, cache, pool), NewSynchronizer(directory, cache, pool)
	defer first.Close()
	defer other.Close()
	// The origin body can outlive a relay's evicted certificate descriptor.
	relay := netgossip.NewPool().NewSession(netgossip.Manifest)
	defer relay.Close()
	have := relay.Offer(payload, now.Add(time.Minute), now)
	requests, _, err := first.Receive(ControlMessage{Kind: "gossip", Gossip: have}, now)
	if err != nil || len(requests) != 1 || requests[0].Gossip == nil {
		t.Fatal(requests, err)
	}
	if queued, _, err := other.Receive(ControlMessage{Kind: "gossip", Gossip: have}, now); err != nil || len(queued) != 0 {
		t.Fatal(queued, err)
	}
	data, _, err := relay.Receive(*requests[0].Gossip, now)
	if err != nil || data == nil {
		t.Fatal(data, err)
	}
	requests, _, err = first.Receive(ControlMessage{Kind: "gossip", Gossip: data}, now)
	if err != nil || len(requests) != 1 || requests[0].Kind != "identity-request" || len(first.pending) != 1 {
		t.Fatal(requests, err)
	}
	missing := NewSynchronizer(directory, cache)
	defer missing.Close()
	replies, _, err := missing.Receive(requests[0], now)
	if err != nil || len(replies) != 1 || replies[0].Kind != "identity-missing" {
		t.Fatal(replies, err)
	}
	if _, _, err = first.Receive(replies[0], now); err != nil || len(first.pending) != 0 {
		t.Fatal(first.pending, err)
	}
	if next := first.gossip.Poll(now); len(next) != 0 {
		t.Fatal("empty supplier retried without new availability", next)
	}
	next := other.gossip.Poll(now)
	if len(next) != 1 || next[0].Op != "want" {
		t.Fatal("alternate waited for timeout", next)
	}
	reply, _, err := relay.Receive(*next[0], now)
	if err != nil || reply == nil {
		t.Fatal(reply, err)
	}
	if _, got, err := other.gossip.Receive(*reply, now); err != nil || !bytes.Equal(got, payload) {
		t.Fatal("alternate could not fetch", err)
	}
}

// A descriptor response and an older queued lookup can cross on a live link.
// If the supplier has since forgotten it, release already-downloaded partial
// bytes without disturbing another principal's independent queued chain.
func TestIdentityMissingReleasesPartialCertificateFetch(t *testing.T) {
	f := newCertificateFixture(t)
	a, b, c := testPrincipal(testTenantUUID, testDeviceAID), testPrincipal(testTenantUUID, testDeviceBID), testPrincipal(testTenantUUID, testDeviceCID)
	source := NewCertificateExchange(f.cache(t, a, nil), testTenant(t))
	defer source.Close()
	parts := NewCertificatePool()
	cache := f.cache(t, b, parts)
	directory, _ := NewDirectory(testTenant(t), 10, cache)
	first, other := NewSynchronizer(directory, cache), NewSynchronizer(directory, cache)
	defer first.Close()
	defer other.Close()
	description := source.Describe(a, f.now)
	requests, _, err := first.Receive(ControlMessage{Kind: "certificate", Certificate: description}, f.now)
	if err != nil || len(requests) != 1 || requests[0].Certificate.Request == nil {
		t.Fatal(requests, err)
	}
	queued, _, err := other.Receive(ControlMessage{Kind: "certificate", Certificate: description}, f.now)
	if err != nil || len(queued) != 0 {
		t.Fatal(queued, err)
	}
	unrelated := DescribeCertificates(c, f.chains[c])
	if _, _, err = first.Receive(ControlMessage{Kind: "certificate", Certificate: unrelated}, f.now); err != nil {
		t.Fatal(err)
	}
	chunk, _, err := source.Receive(*requests[0].Certificate, f.now)
	if err != nil || len(chunk) != 1 || chunk[0].Chunk == nil {
		t.Fatal(chunk, err)
	}
	requests, _, err = first.Receive(ControlMessage{Kind: "certificate", Certificate: chunk[0]}, f.now)
	if err != nil || len(requests) != 1 || requests[0].Certificate.Request.Offset != certificateChunkBytes {
		t.Fatal(requests, err)
	}
	before := parts.bytes
	if _, _, err = first.Receive(ControlMessage{Kind: "identity-missing", Principal: a}, f.now); err != nil {
		t.Fatal(err)
	}
	if first.certificates.waiting != nil || parts.bytes >= before {
		t.Fatal("partial certificate claim retained")
	}
	if _, _, ok := first.certificates.PendingChain(a, f.now); ok {
		t.Fatal("missing descriptor retained")
	}
	if _, _, ok := first.certificates.PendingChain(c, f.now); !ok {
		t.Fatal("unrelated queued chain canceled")
	}
	partRequests, _, err := other.certificates.Poll(f.now)
	if err != nil || len(partRequests) != 1 || partRequests[0].Request == nil || partRequests[0].Request.Offset != 0 {
		t.Fatal("alternate did not restart immediately", partRequests, err)
	}
	completed := 0
	for steps := 0; len(partRequests) > 0; steps++ {
		if steps > 32 {
			t.Fatal("alternate did not finish")
		}
		var responses []*CertificateMessage
		for _, q := range partRequests {
			out, _, err := source.Receive(*q, f.now)
			if err != nil {
				t.Fatal(err)
			}
			responses = append(responses, out...)
		}
		partRequests = nil
		for _, response := range responses {
			out, done, err := other.certificates.Receive(*response, f.now)
			if err != nil {
				t.Fatal(err)
			}
			partRequests = append(partRequests, out...)
			completed += len(done)
		}
	}
	if completed != 1 {
		t.Fatal("alternate completion absent", completed)
	}
}
