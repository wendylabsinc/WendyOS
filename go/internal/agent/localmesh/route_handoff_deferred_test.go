package localmesh

import (
	"bytes"
	"testing"
	"time"
)

func TestRouteHandoffDeferredCertificatePollRequestsRealUpdate(t *testing.T) {
	f := newHandoffActorFixture(t)
	proof := f.lateProof()
	receiver := f.nodes[1]
	p := receiver.peers[1]
	source := f.nodes[0].Credentials
	principal := source.Device.Principal()
	leaf := source.Certificate.Certificate[0]
	hash := certificateHash(leaf)
	// Model an ordinary identity/object-cache miss. The other neighbour then
	// supplies public leaf bytes to the shared pool, without granting identity.
	receiver.cache.mu.Lock()
	delete(receiver.cache.entries, principal)
	receiver.cache.mu.Unlock()
	receiver.cache.parts.mu.Lock()
	if obj, ok := receiver.cache.parts.objects[hash]; ok {
		receiver.cache.parts.bytes -= len(obj.data)
		delete(receiver.cache.parts.objects, hash)
	}
	receiver.cache.parts.mu.Unlock()
	_, changed, err := p.sync.Receive(ControlMessage{Kind: "manifest", Manifest: &proof}, f.now(), 1)
	if err != nil || len(changed) != 0 || len(p.sync.pending) != 1 {
		t.Fatal("manifest did not await identity", err, len(changed), len(p.sync.pending))
	}
	replies, changed, err := p.sync.Receive(ControlMessage{Kind: "certificate", Certificate: DescribeCertificates(principal, source.Certificate.Certificate)}, f.now(), 1)
	if err != nil || len(changed) != 0 || len(replies) != 1 || replies[0].Certificate == nil || replies[0].Certificate.Request == nil {
		t.Fatal("missing certificate did not enter deferred fetch", err, len(changed), replies)
	}
	receiver.cache.parts.put(leaf, f.now().Add(time.Hour), f.now())
	previous := receiver.routeOrigins
	_, changed, err = p.sync.ContinueCertificates(f.now(), 128)
	if err != nil || len(changed) != 1 {
		t.Fatal("real Poll did not verify and admit pending proof", err, len(changed))
	}
	// The maintenance call site must use this same post-admission handler.
	f.links[1].packets = nil
	if err := receiver.afterDirectoryChanges(p, changed, previous, "certificate-repair"); err != nil {
		t.Fatal(err)
	}
	target := source.Device.MeshAddress().As16()
	found := false
	for _, packet := range f.links[1].packets {
		if len(packet) < 5 || packet[0] != PacketBabel {
			continue
		}
		b := packet[1:]
		for off := 4; off < len(b); {
			if b[off] == 0 {
				off++
				continue
			}
			if off+2 > len(b) {
				break
			}
			n := int(b[off+1])
			if off+2+n > len(b) {
				break
			}
			if b[off] == 9 && n == 18 && b[off+2] == 2 && b[off+3] == 128 && bytes.Equal(b[off+4:off+20], target[:]) {
				found = true
			}
			off += 2 + n
		}
	}
	if !found {
		t.Fatal("deferred certificate admission waited for periodic Babel instead of requesting the real matching update")
	}
	f.pump()
	f.advance(6 * time.Second)
	f.assertForwarded(true, 1)
	if len(f.networks[1].interrupted) != 0 {
		t.Fatal("deferred repair interrupted installed forwarding")
	}
}
