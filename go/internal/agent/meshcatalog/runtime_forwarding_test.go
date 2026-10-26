package meshcatalog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestFreshnessRelayDoesNotForwardHolesAfterQueueOverflow(t *testing.T) {
	for _, blockedIndex := range []int{0, 1} {
		t.Run(string(rune('0'+blockedIndex)), func(t *testing.T) {
			f := newFixture(t)
			catalog, _ := f.newCatalog(t, p534, "default", nil, nil)
			r, _ := NewRuntime(catalog, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
			upstream := &catalogPeer{id: 1, out: make(chan []byte, maxInventoryQueueFrames)}
			downstream := &catalogPeer{id: 2}
			r.peers[1], r.peers[2] = upstream, downstream
			query := FreshRequest{Origin: p536, Requester: p533, Nonce: strings.Repeat("a", 64), Hops: 32}
			now := time.Now()
			r.requests[query.Nonce] = &freshRequestState{query: query, upstream: 1, downstream: 2, expires: now.Add(freshLifetime)}
			forwarded := []int{}
			drain := func() {
				for len(upstream.out) > 0 {
					data := <-upstream.out
					upstream.queued.Add(-int64(len(data)))
					var m inventoryMessage
					if err := json.Unmarshal(data, &m); err != nil {
						t.Fatal(err)
					}
					if m.Chunk != nil {
						forwarded = append(forwarded, m.Chunk.Index)
					}
				}
			}
			chunk := func(index int) {
				t.Helper()
				m := inventoryMessage{Message: Message{Kind: "fresh-chunk"}, Chunk: &FreshChunk{Nonce: query.Nonce, Index: index, Total: 3, Data: []byte("data")}}
				if err := r.receiveFresh(downstream, m, now); err != nil {
					t.Fatal(err)
				}
			}
			for index := 0; index < blockedIndex; index++ {
				chunk(index)
				drain()
			}
			// Occupy the real bounded writer queue, then let one response chunk arrive.
			for range maxInventoryQueueFrames {
				if !r.queue(upstream, inventoryMessage{Message: Message{Kind: "fresh-request"}, Request: &query}) {
					t.Fatal("failed to fill bounded queue")
				}
			}
			chunk(blockedIndex)
			drain()
			for index := blockedIndex + 1; index < 3; index++ {
				chunk(index)
				drain()
			}
			for expected, index := range forwarded {
				if index != expected {
					t.Fatalf("relay forwarded chunk %d after losing chunk %d; requester would reject the healthy neighbour", index, expected)
				}
			}
			// A new nonce may recover normally once the writer has room again.
			query.Nonce = strings.Repeat("b", 64)
			r.requests[query.Nonce] = &freshRequestState{query: query, upstream: 1, downstream: 2, expires: now.Add(freshLifetime)}
			forwarded = nil
			for index := 0; index < 3; index++ {
				chunk(index)
				drain()
			}
			if len(forwarded) != 3 {
				t.Fatal("new freshness response did not recover after queue pressure")
			}
		})
	}
}
