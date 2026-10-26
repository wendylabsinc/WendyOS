package meshcatalog

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestFreshnessProofFetchesMissingCertificatesWithoutEmbeddingBundle(t *testing.T) {
	f := newFixture(t)
	origin, originCache := f.newCatalog(t, p533, "default", nil, nil)
	requester, cache := f.newCatalog(t, p534, "default", nil, nil)
	r, err := NewRuntime(requester, func() localmesh.NodeSnapshot {
		return localmesh.NodeSnapshot{
			Devices: []localmesh.Manifest{{Device: origin.device.UUID(), Expires: f.now.Add(time.Minute).UnixMilli()}},
			Routes:  []babel.Route{{Prefix: netip.PrefixFrom(origin.device.MeshAddress(), 128), Link: 1}},
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	p := &catalogPeer{id: 1, device: origin.device, syncer: NewSynchronizer(requester, cache), inventory: receivedInventory{done: true}, started: f.now, out: make(chan []byte, maxInventoryQueueFrames)}
	r.peers[1] = p
	r.checkFreshness(f.now)
	var request inventoryMessage
	if err := json.Unmarshal(<-p.out, &request); err != nil || request.Request == nil {
		t.Fatal("missing original freshness challenge", err)
	}
	proof, err := origin.signInventory(*request.Request, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(proof, []byte(`"bundle"`)) {
		t.Fatal("freshness proof still embeds a certificate bundle")
	}
	for _, certificate := range origin.creds.Certificate.Certificate {
		if bytes.Contains(proof, []byte(base64.StdEncoding.EncodeToString(certificate))) {
			t.Fatal("freshness proof contains full certificate bytes")
		}
	}
	if _, _, ok := cache.Get(origin.principal, f.now); ok {
		t.Fatal("test must begin with a genuinely cold origin identity")
	}
	chunks := (len(proof) + freshChunkBytes - 1) / freshChunkBytes
	for index, offset := 0, 0; offset < len(proof); index++ {
		end := min(len(proof), offset+freshChunkBytes)
		m := inventoryMessage{Message: Message{Kind: "fresh-chunk"}, Chunk: &FreshChunk{Nonce: request.Request.Nonce, Index: index, Total: chunks, Data: proof[offset:end]}}
		if err := r.receiveFresh(p, m, f.now); err != nil {
			t.Fatal(err)
		}
		offset = end
	}
	if r.ProjectionReady(origin.device) {
		t.Fatal("proof was projected before its missing certificates were verified")
	}
	if len(p.out) == 0 {
		t.Fatal("cold freshness proof did not request its missing certificate")
	}
	source := NewSynchronizer(origin, originCache)
	for steps := 0; len(p.out) > 0; steps++ {
		if steps > 1000 {
			t.Fatal("certificate fetch did not terminate")
		}
		var query inventoryMessage
		if err := json.Unmarshal(<-p.out, &query); err != nil {
			t.Fatal(err)
		}
		if query.Kind != "certificate" || query.Certificate.Request == nil {
			t.Fatal("expected element request, not another challenge or whole bundle", query)
		}
		replies, _, err := source.Receive(query.Message, f.now)
		if err != nil {
			t.Fatal(err)
		}
		for _, reply := range replies {
			data, err := inventoryWire(inventoryMessage{Message: reply})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.receive(p, data); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Runtime.receive uses the real clock for received certificate messages;
	// keep the finishing tick after those cache writes.
	r.checkFreshness(time.Now())
	if !r.ProjectionReady(origin.device) || len(r.requests) != 0 {
		t.Fatalf("same nonce did not complete after authenticated element fetch: fresh=%v pending=%v requests=%d", r.fresh, r.pendingFresh, len(r.requests))
	}
}
