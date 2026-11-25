package liveapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Independent sources can poll at subsecond intervals for fusion without
// increasing the mutation rate. The real HTTP and Unix RPC path stays bounded.
func TestFastSourcePollKeepsPublisherRPCPacing(t *testing.T) {
	var calls atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(snapshot(t, calls.Add(1)))
	}))
	defer server.Close()
	api, rpc := rpcClient(t, "gas-1", "")
	cfg := runConfig(t, server.URL)
	cfg.PollIntervalMS, cfg.MaxSamples = 50, 4
	started := time.Now()
	out, err := Run(context.Background(), cfg, api)
	if err != nil || out.Status != "complete" || len(out.Receipts) != 4 {
		t.Fatal("bounded fast polling failed", err)
	}
	if time.Since(started) < 3*125*time.Millisecond {
		t.Fatal("faster polling bypassed independent RPC pacing")
	}
	rpc.mu.Lock()
	defer rpc.mu.Unlock()
	if len(rpc.requests) != 4 || len(rpc.puts) != 0 {
		t.Fatal("fast polling changed publication count or modality")
	}
	for i, r := range out.Receipts {
		if r.NativeSequence != uint64(i+1) {
			t.Fatal("faster polling rewrote original capture identity")
		}
	}
}
